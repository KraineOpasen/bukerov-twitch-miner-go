# ADR-0003: Stable-native deterministic skill-update foundation

- **Status**: Accepted
- **Date**: 2026-08-27
- **Scope**: G1.1 only
- **Supersedes**: donor `docs/adr/0003-automated-skill-update-candidates.md` from
  `main@9c2c11030dd34c34bd7812e5a18bfe52d897b2a7`; this stable adaptation replaces its
  default-branch control assumptions without importing donor authority

## Context

The repository's audited skills authority is the live stable line, currently `release/0.3`: 81 vendored
skills from six providers, with each provider's policy, file-level manifest, and patch ledger owning its
reviewed bytes and pin. The historical deterministic updater exists on `main`, which is still the live
repository default branch. Scheduled workflows run from the default branch, so that updater proposes
main-based state and does not maintain the non-default stable authority.

Copying the whole main control plane onto stable would replace stable-owned CI, validator, policies,
manifests, routing, and installed-skill truth. Activating publication at the same time would also require
write permissions, mutation identity, liveness, review, and promotion decisions that have not been
commissioned. The foundation therefore has to move the deterministic detector/preparer boundary onto
stable without pretending that later autonomous stages already exist.

## Decision

Adopt a new stable-only workflow at `.github/workflows/stable-skills-maintenance.yml` and the bounded
deterministic engine under `scripts/skill_updates/`. `main` is donor data only; the stable policies,
provider/plugin registries, manifests, patch ledgers, routing, validator, CI release targeting, and
installed 81-skill inventory remain authoritative.

G1.1 is **artifact-only**:

- the workflow proves stable branch, event ref, workflow source, repository/default-branch facts,
  committed policy, quarantine, runtime, and dependency identity before upstream access;
- upstream repositories are treated as data in isolated bare git repositories and no fetched content
  is executed;
- candidate bytes and their deterministic report are written only below an artifact root outside the
  checkout;
- the only production terminal states are `NO_DRIFT`, `BLOCKED`, and
  `PREPARED_AUDIT_REQUIRED`; unknown or unavailable evidence fails closed and cannot become PASS or
  `NO_DRIFT`;
- no production caller can reach ref/PR/issue publication, model review, `AUDITED`, Ready, merge,
  auto-merge, promotion, or sibling installation.

The workflow uses an exact reviewed `actions/checkout` commit with a closed source/tree/licence/runtime
and input envelope, checks out the trusted event SHA with persisted credentials disabled, and runs with
read-only contents permission and no secret, App, PAT, OIDC, or model credential. Runtime and dependency
facts are machine-owned under `docs/agents/skills-maintenance/` and fail closed on mismatch.

The historical pull requests #223, #233, and #241 and issues #230, #238, #239, and #240 form a closed,
machine-validated legacy quarantine. G1.1 may observe their identity but cannot mutate, duplicate,
supersede, adopt, or use them as promotion evidence.

At adoption, `main` remains the live default and `release/0.3` remains non-default. The new workflow is
recorded as **UNCOMMISSIONED**, full maintenance liveness as **UNAVAILABLE**, and the external heartbeat
as **UNCOMMISSIONED**. A detector-only success cannot establish a healthy future control plane.

## Consequences

- Stable owns reproducible drift detection and candidate preparation without changing audited skill
  bytes, pins, routing, provider authority, or product behaviour.
- A prepared artifact is explicitly unaudited and ephemeral; it is evidence for a later, separately
  authorized audit, not repository state.
- The new schedule is not claimed active while stable is non-default.
- Publication code may remain testable library code, but its production reachability is denied and
  mechanically tested.
- G1.2+ semantic reviewers, publisher/App identity, permissions, promotion quorum, Ready/merge/auto-merge,
  sibling decisions, and full liveness orchestration remain deferred.
- After merge, disabling/draining the historical main control plane, switching the default branch to
  `release/0.3`, enabling the new workflow if necessary, and selecting/configuring external heartbeat and
  full liveness are separate owner-gated actions.

## Rejected alternatives

- **Keep the updater only on main:** it continues to prepare state against the wrong development
  authority.
- **Broadly transplant main:** it overwrites stable-owned governance and CI truth and imports unrelated
  control-plane assumptions.
- **Publish candidates in G1.1:** it requires write authority and mutation/liveness closure belonging to
  later stages.
- **Treat detector success as commissioning:** it reports a partial mechanism as full-control-plane
  health and hides absent reviewer/publisher/heartbeat stages.

## Links

- [`docs/agents/skills-update-automation.md`](../agents/skills-update-automation.md)
- [`docs/agents/skills-routing.md`](../agents/skills-routing.md)
- [`docs/agents/skills-maintenance/policy.json`](../agents/skills-maintenance/policy.json)
- [`docs/agents/skills-maintenance/control-plane.json`](../agents/skills-maintenance/control-plane.json)
- [`docs/agents/skills-maintenance/legacy-quarantine.json`](../agents/skills-maintenance/legacy-quarantine.json)
- [`docs/agents/skills-maintenance/external-dependencies.json`](../agents/skills-maintenance/external-dependencies.json)
