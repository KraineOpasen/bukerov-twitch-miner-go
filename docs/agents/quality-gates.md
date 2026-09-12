# Quality gates

Four gates, increasing in scope — the repo-native elaboration of `GOVERNANCE_V3.md` §12. A finding from
any gate is development feedback: diagnose and repair inside the same active task, rerun, and repeat until
the gate honestly passes — a failure is never reported as a pass, and a test is never weakened, skipped,
or narrowed to reach green. Only a repair strategy exhausted without an honestly passing final gate — an
integrity/authority failure, not an ordinary red test — drops the session to `READ_ONLY` (see
`GOVERNANCE_V3.md` §5 and `docs/agents/operation-modes.md`).

## Q0 — Compiles / parses

The change is syntactically valid. For Go: `go build ./...` and `go vet ./...` on the touched packages. For
config/data files: the relevant parser succeeds (`python3 -m json.tool`, YAML frontmatter round-trips, etc.).

## Q1 — Targeted tests

Tests for the touched package(s) pass: `go test -v -race ./internal/<pkg>/...`. For non-Go governance/tooling
changes, the equivalent self-test passes (e.g. `python3 .claude/hooks/governance-policy.py --self-test`,
`python3 scripts/validate-agent-governance.py --application-scope generic`).

The oracle and golden-check distinctions below hold for any Q1 evidence; the classified-failure and
reached-case distinctions hold wherever a mutation proof is claimed. Each distinction that bears on a claim
must hold for the claim to be evidentiary rather than merely plausible. `GOVERNANCE_V3.md` §12 requires
mutation/harness proof *where applicable*; this elaboration reads that scope as proportional to the risk
carried by the code being changed, and never as a mandate to mutate the code behind every test or every
change. Where a **disposable mutation** proof is claimed, §12's invariant
(`baseline PASS → mutation → expected FAIL → byte-identical restore → PASS → clean`) is the authority.

- **Independent oracle.** The expected behaviour or value comes from an approved requirement/spec, an
  exact/golden artifact, or an independently derived calculation or model — never from the implementation
  under test. A golden regenerated from the implementation it checks is that implementation, not an
  independent oracle.
- **Classified failure.** The expected FAIL is a behavioural failure attributable to the intended property —
  an assertion failure, or race-detector, panic or explicit property-failure evidence attributed to the
  intended fault; a harness timeout panic is a timeout, not a panic. A compile or build failure, a timeout
  or non-completion, no matching test, an unreached or inaccessible path, an unrelated earlier
  refusal/failure, and tooling that is unavailable are each reported as themselves (§6) and none of them is
  a behavioural kill; a build break is not behavioural proof, so a mutation that fails to build establishes
  no behavioural test protection and is replaced rather than counted. A timeout or non-completion is never
  itself the kill: a liveness claim names the property and records evidence of the case that exercises it,
  the detection mechanism, and the property violation attributable to the intended fault — declaring a
  timeout to be a liveness signal is not that evidence. Survival alone classifies nothing: only a mutant
  argued to preserve behaviour over the reachable input domain is **equivalent**, its survival is then not
  evidence of a weak test, and the argument is recorded with it.
- **Reached case, named assertion.** A test that merely ran proves nothing about the claimed case or
  dimension. Evidence shows the intended case reached the mutation site, and the assertion/property claimed
  to protect it is named among those observed to fail for a relevant *compiling* fault — a guard that stays
  green either way is vacuous for the claim.
- **Golden checks survive round-trips.** A self-consistent encoder/decoder round trip does not establish the
  required wire representation. Added round-trip coverage keeps the independent exact/golden-format checks
  for the same representation, wherever they live.

## Q2 — Full regression

Runs on the final candidate only, on the integrated tree, at the SHA being published (`GOVERNANCE_V3.md`
§12): the whole module's test suite with the race detector — `TZ=UTC go test -race -count=1 ./...` (the
final-gate form of the everyday `go test -v -race ./...` from `CLAUDE.md`/`.claude/rules/tests.md`, made
deterministic with `-count=1` and a pinned timezone) — plus `go mod verify`, `go vet ./...`,
`go build ./...`, `make lint`, and proof that only the intended paths changed. Development iteration does
not re-run this full gate.

## Q3 — Review

Review axes per `GOVERNANCE_V3.md` §12 — Standards; Spec/domain compliance; differential/caller impact;
security/concurrency; provenance; browser/a11y when UI is touched — run independently (a
`code-review`-style pass covers the first two), read-only, findings reported not auto-fixed, before a
change moves to PUBLISH_DRAFT.

## Governance/tooling change-sets

For a change-set that touches only the governance layer (`CLAUDE.md`, `GOVERNANCE_V3.md`, `.claude/**`,
`CONTEXT.md`, `docs/agents/**`, `docs/adr/**`, `scripts/validate-agent-governance.py`) and no application
paths, Q0/Q1 are: `python3 -m json.tool` on every touched JSON file, the hook self-test
(`python3 .claude/hooks/governance-policy.py --self-test`), and the governance validator with its own
fixture matrix. Generic repository validation is
`python3 scripts/validate-agent-governance.py --application-scope generic`; a G1/stable-skills
change-set must additionally use its exact reviewed task base:

```bash
GOVERNANCE_BASE_SHA=$BASE_SHA \
  python3 scripts/validate-agent-governance.py --application-scope g1-stable-skills
```

The fixture matrix remains the separate
`python3 scripts/validate-agent-governance.py --self-test` invocation. The heavy Go gates apply only
where changed-path analysis shows application paths are affected; mutation testing is not applicable
to Markdown/governance content.
