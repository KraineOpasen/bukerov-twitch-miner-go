# Stable-native deterministic skill maintenance (G1.1)

This document defines the stable-owned, artifact-only foundation for detecting upstream skill drift and
preparing deterministic evidence for a later audit. It does not define an autonomous updater.

- **Workflow:** `.github/workflows/stable-skills-maintenance.yml`
- **Engine:** `scripts/skill_updates/`
- **Entry points:** `scripts/check-skill-updates.py` and `scripts/prepare-skill-update.py`
- **Provider registry:** `docs/agents/skills-update-providers.json`
- **Plugin inventory:** `docs/agents/skills-update-plugins.json`
- **Machine policy:** `docs/agents/skills-maintenance/policy.json`
- **Control plane:** `docs/agents/skills-maintenance/control-plane.json`
- **Legacy quarantine:** `docs/agents/skills-maintenance/legacy-quarantine.json`
- **Runtime/dependency closure:** `docs/agents/skills-maintenance/external-dependencies.json`
- **Decision:** `docs/adr/0003-stable-native-deterministic-skill-updates.md`

`scripts/prepare-skill-update.py` is retained byte-for-byte from its pinned donor provenance;
its historical module synopsis is non-authoritative. The current stable-owned CLI rejects
`--publish` as `UNCOMMISSIONED` and preparation requires both an exact `--target-sha` and an
explicit outside-checkout `--artifact-root`.

## Current status: stable-owned but uncommissioned

At G1.1 adoption, `release/0.3` is the committed stable policy base but is still a **non-default**
branch; the live repository default remains `main`. GitHub scheduled workflows run from the default
branch, so merely committing the workflow on `release/0.3` does not make its schedule effective.

The stable workflow is therefore **UNCOMMISSIONED** and full control-plane liveness is **UNAVAILABLE**.
The external heartbeat is also **UNCOMMISSIONED**. These are intentional, machine-recorded facts, not an
outage and not a claim that the historical main updater maintains the stable line. A later default-branch
migration and liveness commissioning require separate owner actions. No manual workflow dispatch,
settings change, enable/disable action, or default-branch switch is performed as part of G1.1.

## Boundary

G1.1 does only these things:

1. prove repository, stable branch, selected ref, workflow source, committed policy, live default branch,
   fetched default head, runtime, and external-dependency identity;
2. resolve reviewed provider refs to full 40-hex commits;
3. classify drift using pinned ancestry, inventory, provenance, licence, authority, closure, and patch data;
4. prepare bounded candidate bytes and a deterministic report under an artifact root outside the checkout;
5. report terminal state in the workflow summary and reconcile the complete G1.1 detector/preparer run.

G1.1 does **not**:

- create, update, push, or delete a git ref;
- create, update, close, reopen, retarget, supersede, duplicate, or comment on a pull request or issue;
- upload or publish a candidate artifact as repository state;
- invoke a model or make a semantic review decision;
- claim `AUDITED`, semantic PASS, Ready-for-review, merge eligibility, or promotion authority;
- merge, enable auto-merge, release, tag, deploy, or change repository settings;
- install, exclude, hold, route, or otherwise decide the fate of a sibling skill;
- change installed `.claude/skills/**` bytes, provider pins, provider authority, or the audited
  81-skill/six-provider baseline.

Publication adapters remain importable and are exercised only by isolated regression tests with
`FakeGitHub` and local temporary remotes. No operational production route or authority, including the
stable workflow, can reach them. G1.2 and later reviewer, publisher, promoter, application identity,
permission, and auto-merge capabilities are deferred.

## Terminal states

Every production provider result ends in exactly one of these states:

| State | Meaning | Permitted effect |
| --- | --- | --- |
| `NO_DRIFT` | Every required identity and evidence check succeeded and the reviewed ref still resolves to the pinned commit. | Report only; no candidate. |
| `BLOCKED` | Drift or control-plane evidence requires judgement, is inconsistent, or is unknown/unavailable. | Record deterministic evidence; no partial candidate and no mutation. |
| `PREPARED_AUDIT_REQUIRED` | A proven fast-forward produced a mechanically bounded candidate. It has not been semantically reviewed. | Write candidate bytes and `candidate-report.json` only under the external artifact root. |

`DRIFT_DETECTED`, discovery markers, evaluation markers, and internal planning values may describe
intermediate evidence, but they are not additional production terminal states. Automation has no
transition to `AUDITED`. Unknown or unavailable repository, workflow, ref, upstream, quarantine,
runtime, or dependency facts are never converted to PASS or `NO_DRIFT`; they fail closed as `BLOCKED`
or stop before upstream access.

## Detection and preparation

The check entry point is read-only with respect to the repository and GitHub subjects:

```bash
python3 -I -S -B scripts/check-skill-updates.py \
  --provider all \
  --json-out /tmp/stable-skills-report.json \
  --summary /tmp/stable-skills-summary.md
```

The plan consumes that report and selects only exact `PREPARED_AUDIT_REQUIRED` results. Each matrix item
binds an allowlisted provider key to the full target commit. Preparation re-resolves and verifies that
target; a caller cannot use `--target-sha` to select an arbitrary commit.

```bash
python3 -I -S -B scripts/prepare-skill-update.py \
  --provider mattpocock \
  --target-sha 0123456789abcdef0123456789abcdef01234567 \
  --artifact-root /tmp/stable-skills-mattpocock \
  --summary /tmp/stable-skills-summary.md
```

The artifact root must resolve outside the repository and outside its `.git` directory. Candidate files
preserve repository-relative paths beneath that root, and `candidate-report.json` records the stable base,
provider, old and target commits, state, paths, and evidence. The authoritative checkout is not changed.
On a hosted runner these files are ephemeral unless a later, separately governed stage deliberately
transfers them; G1.1 does not add an upload action.

For each upstream-origin file the preparer compares three byte strings:

- **BASE:** bytes at the old pinned upstream commit;
- **OURS:** the currently audited stable bytes, including recorded local patches;
- **THEIRS:** bytes at the newly resolved upstream commit.

Unchanged sides are selected by byte equality. When both sides changed, the deterministic three-way
merge either yields bounded bytes or blocks; conflict markers are never written. Provenance fields are
recomputed from actual bytes, while judgement fields remain review-owned. A candidate manifest carries
an `automated_candidate` marker, so copying it into the repository cannot masquerade as an audited pin.

## Fail-closed classification

Only a proven fast-forward from the reviewed commit can reach `PREPARED_AUDIT_REQUIRED`. The engine
blocks on, among other things:

- an unresolvable ref or commit, short or malformed identity, wrong upstream default ref, or a selected
  target that no longer equals the reviewed ref;
- diverged, rewritten, reset, or unreachable history;
- installed-skill addition, deletion, rename, or closure/inventory change requiring a decision;
- licence presence or bytes changing;
- a symlink, submodule, newly executable file, mode violation, or out-of-root path;
- trigger/frontmatter authority drift;
- binary dual-sided change, merge conflict, or a local patch that no longer maps;
- vendored bytes or manifests that do not match recorded hashes;
- wrong repository/default/stable/workflow identity, corrupted quarantine, or unproved runtime/dependency
  envelope.

A newly discovered sibling is evidence only. G1.1 never turns it into an `INSTALL`, `EXCLUDE`, or `HOLD`
decision and never widens the installed or routed authority surface. Behaviour-sensitive changes can be
flagged for a future evaluation, but no evaluation or model call occurs in this stage.

## Upstream and runtime security

Fetched upstream repositories are untrusted data. They are fetched into isolated bare repositories and
read with bounded git object commands; no upstream checkout, hook, filter, submodule, LFS object, script,
or executable is run. Git protocol/config, hooks, prompts, credentials, and command arguments are bounded
by the runtime layer. Network access, TLS endpoints, runner, Python, git, and executable ownership are
closed in `external-dependencies.json`; no package is installed at runtime.

Every production Python entry point runs as `python3 -I -S -B`: isolated mode ignores ambient
`PYTHONPATH` and user site configuration, `-S` prevents `site`/`.pth` startup imports, and `-B` prevents
bytecode writes. The pre-checkout gate records the exact isolated-process
`platform.python_version()` evidence. Before any
donor Git call, the adapted stable CLI verifies the pinned donor hardening vector, adds empty credential
helper, required TLS verification, redirect rejection, and an exclusive empty `0700` home, then restores
the process environment on exit. The copy-verbatim donor engine remains byte-identical.

The workflow has no secret, App, PAT, OIDC, or model credential. Top-level permissions are empty and the
detector/preparer jobs receive only `contents: read`. Checkout uses the exact reviewed
`actions/checkout@11d5960a326750d5838078e36cf38b85af677262` identity and its machine-validated source,
tree, licence, bundled-runtime, and input closure. It explicitly selects `${{ github.sha }}` and disables
persisted credentials, submodules, LFS, tag fetching, progress, and global safe-directory mutation.
The three pre-checkout bootstrap bodies are byte-identical and bound by one reviewed SHA-256; unexpected
`GIT_*` configuration/TLS/trace variables and provider-evidence environment shadowing fail before the
Action runs. Immediately after checkout, the runtime uses its closed Git argv/environment to require a
real worktree and prove local `HEAD == GITHUB_SHA`. The Action's REST-download fallback therefore cannot
enter updater or provider logic.

The selected event SHA is trusted only after the runtime proves all committed and live identity facts.
A scheduler, writer, or secret-bearing job must never check out PR or candidate bytes. G1.1 has no writer
or secret-bearing job; untrusted pull-request CI remains read-only and receives no privileged credential.

## Legacy quarantine

The new stable control plane quarantines these exact historical subjects:

- pull requests **#223**, **#233**, and **#241**;
- issues **#230**, **#238**, **#239**, and **#240**.

The machine file binds their complete repository/kind/number/identity facts. Live provider/target
evidence is accepted only from one exact, uniquely anchored Markdown table row for `provider` and one
exact row for `target commit` (or #240's `seen at commit`); missing, duplicate, relocated, malformed, or
conflicting rows fail closed. Incidental strings elsewhere in the GitHub response are not evidence. The
stable workflow may observe the subjects only to prove identity and report evidence. It may not update,
close, reopen, retarget,
supersede, duplicate, comment on, recreate, adopt as authority, or use any of them as promotion evidence.
A live identity mismatch fails closed. G1.1 neither mutates these subjects nor treats their historical
candidate or blocked state as stable audit evidence.

## Liveness and later commissioning

A successful detector or preparer job proves only the bounded G1.1 run at that event SHA. It does not
prove that a future reviewer/publisher/promoter exists, that the scheduler is active on the live default
branch, or that the complete future maintenance control plane is healthy. Detector-only success must not
set a full-control-plane heartbeat or clear `UNCOMMISSIONED`.

After G1.1 is merged, migration is a separate owner-only maintenance window. Nothing in G1.1 performs,
approves, or partially begins it. The owner must use a fresh live readback, an explicit recovery record,
and one mutation authority. Unknown, unavailable, stale, or contradictory evidence is `STOP`; it is never
permission to infer a setting or continue.

Before the first owner mutation, record the repository numeric/full identity, exact merged stable head,
live default and head, every workflow id/path/trusted-file blob/state, queued/in-progress/recent runs,
repository settings, auto-merge state, branch protections and rulesets (including required checks and
bypass actors), PR bases, Apps/webhooks/integrations, and the exact seven quarantined subjects. Hash and
retain that recovery record outside the mutation session. If any required readback or rollback value is
unavailable, if quarantine differs, if another mutation window is active, or if the merged bytes have
drifted, stop before mutation.

The migration order is closed. Each numbered action has a readback barrier; action *n+1* is forbidden
until action *n* and its barrier are proved:

1. `REVERIFY_MERGED_STABLE_AND_LIVE_MIGRATION_INPUTS` — re-prove the repository identity, exact merged
   `release/0.3` head, ancestry, current default/head, G1.1 policy/control hashes, full workflow set, and
   zero conflicting owner operation. **Barrier:** all facts equal the approved migration tuple. Drift,
   missing facts, or a competing operation is `STOP`.
2. `SNAPSHOT_COMPLETE_WORKFLOW_RUN_SETTINGS_RULESET_INTEGRATION_AND_QUARANTINE_STATE` — capture the full
   reversible pre-state described above, including opaque ids and exact blobs rather than path guesses.
   **Barrier:** the snapshot is complete, immutable, hashed, independently readable, and contains every
   value needed for reverse restoration. `UNKNOWN`/`UNAVAILABLE` is `STOP`.
3. `DISABLE_DRAIN_AND_PROVE_ZERO_HISTORICAL_OR_DYNAMIC_MUTATION` — prevent new historical/default-main
   updater and release runs, drain already queued or running work, and enumerate every dynamic publisher
   or integration. Do not cancel a run without recording its terminal state. **Barrier:** zero queued,
   in-progress, or still-triggerable historical/dynamic mutation remains. Any residual writer is `STOP`.
4. `FREEZE_MAIN_AND_NORMALIZE_STABLE_PROTECTION_WITHOUT_A_GAP` — freeze mutation of `main`, then establish
   and read back the owner-approved `release/0.3` ruleset/protection and required checks while the old
   protection is still effective. Never relax both branches at once, never accept a missing required
   check, and keep auto-merge disabled. **Barrier:** both the frozen old authority and protected new
   authority are simultaneously proved; any protection gap is `STOP` and invokes rollback.
5. `CHANGE_DEFAULT_BRANCH_TO_RELEASE_0_3_WITH_AUTO_MERGE_DISABLED` — perform the one default-branch
   switch only after the no-gap barrier. **Barrier:** an immediate live readback says the default is
   exactly `release/0.3` at the approved head and auto-merge is still disabled. A different ref/head or
   an implicit setting change requires immediate rollback; do not continue forward.
6. `VERIFY_POST_SWITCH_DEFAULT_RULES_PR_BASES_WORKFLOW_AND_INTEGRATION_STATE` — compare the complete live
   post-switch state with the approved target: default/head, protections/rulesets/checks, open PR bases,
   workflow inventory, Apps/webhooks/integrations, and unchanged quarantine. No PR is silently retargeted
   and no legacy subject is adopted. **Barrier:** every expected equality and intentional delta is
   accounted for; unexplained drift is `STOP`.
7. `VERIFY_NEW_WORKFLOW_REGISTRATION_AND_ENABLEMENT` — resolve the newly registered workflow by exact id,
   path, trusted-file blob, and state. If GitHub registered it disabled, the owner may enable that exact
   identity only after the preceding barriers. Do not manually dispatch or rerun it. **Barrier:** the
   closed workflow set (`ci.yml` and `stable-skills-maintenance.yml`) is active and digest-correct, and
   the next natural schedule is the only accepted liveness source.
8. `SELECT_CONFIGURE_AND_TEST_EXTERNAL_FULL_ORCHESTRATOR_HEARTBEAT` — separately approve a public-API,
   read-only monitor, account/configuration, rate/cost envelope, channel, closed workflow set, incident
   dedupe, and recovery fixtures. Test disabled/missing/digest-mismatch, stale/wrong-head, terminal
   failure, detector-only, grace expiry, and recovery cases. **Barrier:** commissioning remains false
   until a natural `schedule` run is `completed/success`, the YAML key `reconcile-all` is proved by the
   trusted workflow blob to map to the externally observable API job name
   `Reconcile complete G1.1 control plane` and that API job is `success`,
   the run's workflow id, path, and trusted-file blob equal the stable orchestrator in the live closed
   workflow set, the head is the live default (or a proved ancestor with identical workflow and aggregate
   control-plane identity), every closed-set workflow is active/digest-correct, and the result is within
   48 hours.

Rulesets, settings, Apps, secrets, heartbeat selection, permission widening, default switching, manual
dispatch, and every G1.2+ implementation are owner actions outside G1.1. A detector-only completion,
workflow enablement, or default switch alone never establishes liveness.

## Verification

The stable CI governance job owns the repository checks. The focused local commands are:

```bash
python3 -I -S -B scripts/validate-agent-governance.py
python3 -I -S -B scripts/validate-agent-governance.py --self-test-hook
python3 -I -S -B scripts/skill_updates/runtime.py verify-repository --repo-root .
python3 -I -S -B scripts/validate-agent-governance.py --self-test
python3 -I -S -B scripts/skill_updates/tests/mutation_probe.py --check-anchors
python3 -I -S -B -m unittest discover -t scripts -s scripts/skill_updates/tests
```

The disposable mutation probe additionally proves that load-bearing policy, identity, permission,
runtime, dependency, quarantine, and publication-reachability mutations fail and that the clean bytes
pass again after restoration. The existing provider manifests, policies, and patch ledgers remain the
authority for the 81 installed skills; passing a detector test never changes their audit status.

## Rollback

Rollback is an owner-only maintenance window using the exact pre-migration recovery record. It is not a
workflow feature and must not guess missing settings. First stop any health claim, snapshot the current
failed/post-migration state, preserve incidents and run evidence, and prove one rollback authority. Then
reverse the eight migration actions with the same readback discipline:

1. Decommission or silence the external heartbeat configuration from action 8 without emitting a
   synthetic healthy/recovery message; retain its account/config, dedupe keys, and evidence for audit.
2. Prevent new stable-maintenance schedules, drain every queued/running stable orchestrator, and disable
   only the exact workflow identity enabled in action 7. Never cancel or replace evidence silently.
3. Reverse only the intentional PR-base/integration/rules deltas recorded by action 6; do not retarget a
   PR, mutate an App/webhook, or restore a setting unless its exact before-value is in the recovery record.
4. Before reversing action 5, restore and read back the snapshotted `main` protections/required checks
   while `release/0.3` protection remains effective. Then switch the default back to the exact recorded
   `main` head with auto-merge disabled. This protective overlay is the no-gap barrier even though the
   semantic action order is being reversed.
5. Read back default/head, rulesets, required checks, auto-merge, PR bases, workflows, integrations, and
   quarantine. Only after `main` is again protected default may action 4's temporary stable normalization
   be reduced to its exact snapshotted value; never leave either authority unprotected during handoff.
6. Re-enable only the exact historical workflow ids/blobs/states captured before action 3, and only after
   proving zero stale stable run can still mutate. Do not dispatch or rerun either generation. Let later
   natural triggers reconcile missed upstream observations; delay never becomes `NO_DRIFT`.
7. Compare the restored complete state byte/fact-for-fact with the action-2 snapshot and retain a signed
   rollback result plus every intentional residual. Any unexplained difference remains an incident.
8. Re-run the action-1 repository/head/control/quarantine verification against the restored authority.
   Do not claim rollback complete until every required readback is exact.

At every reverse barrier, a missing recovery value, active writer, workflow id/blob mismatch, protection
gap, wrong default/head, quarantine change, or inability to restore an integration is `STOP`. Hold the
safest already-frozen state and escalate to the owner; do not continue, improvise, manually dispatch,
weaken protections, enable auto-merge, or recreate historical subjects. Rollback never changes provider
pins, installed skill bytes, product runtime behaviour, or the state/content of quarantined PRs/issues.
