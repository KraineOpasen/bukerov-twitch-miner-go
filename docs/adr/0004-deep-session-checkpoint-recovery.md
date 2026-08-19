# ADR-0004: Durable DEEP-session checkpoint and recovery

- **Status**: Accepted
- **Date**: 2026-08-19

## Context

Governance v3 (ADR-0002) settled *authority* — a task contract, four operation modes, quality gates, and a
session boundary that resets every new session to READ_ONLY. It deliberately said nothing about *continuity*.
That was the right scope at the time, and it left a real gap: a DEEP task — one that runs long enough to be
compacted, interrupted, or moved between environments — had no defined way to survive an interruption.

The gap is not theoretical. Sessions in this project run in an ephemeral container: the repository is cloned
fresh at start, and the container is reclaimed after inactivity. Anything on local disk is gone at the next
session, and nothing in `/tmp` or `.scratch/` crosses a workspace boundary at all.

Three recovery mechanisms already existed, and none of them closes this gap:

- **`handoff`** writes a document into a gitignored `.scratch/` directory or the session scratchpad. Its own
  local patch says exactly that. Machine-local by construction, so it does not survive a reclaimed container.
- **`ce-handoff`** is richer — a managed store, a `ce-handoff/v1` frontmatter contract, a disciplined resume
  that treats the source as untrusted and stops before acting. But its default store is OS-managed temporary
  storage, which the skill itself warns is not permanent, and its granularity is a *session handoff to another
  agent*: prose orientation, pointer-first, explicitly not a machine-checkable record of which gates passed at
  which SHA.
- **`ce-work`** recovers *inside* a run. It has no concept of a session that ended.

So the wrong-granularity problem and the machine-local problem compound: the artifact that would survive is
not precise enough to prevent repeated work, and the artifact that is precise enough does not survive.

Meanwhile the failure mode on the other side is worse than repeated work. A continuity document that restates
"mode: PUBLISH_DRAFT, allowed_paths: ..." is one paste away from a fresh session reading its own authority out
of a file instead of out of a live owner contract. Governance v3's session boundary exists precisely to stop
that, and any continuity mechanism has to be built so it cannot erode it.

## Decision

Adopt a checkpoint/recovery protocol, documented normatively in `docs/agents/session-recovery.md`.

### 1. The canonical carrier is a user-visible `deep-checkpoint/v1` block

At meaningful task boundaries the session emits one fenced, schema-versioned block **in its answer**. That
text is the canonical interchange format: complete on its own, pasteable, and readable by a session that has
none of the previous session's files.

It is chosen because it is the only artifact that crosses every interruption class this project actually has,
including a reclaimed container and a fresh workspace on another machine. It also requires **no tracked-file
mutation and no GitHub mutation**, which means a READ_ONLY session can emit one — the state most in need of
being recoverable is the state with the least authority to write anything down.

### 2. The `.scratch/` mirror is opportunistic

The same block may be mirrored to `.scratch/checkpoints/<task_id>.md` for same-workspace convenience. It is
documented as ephemeral, never as durable, and a mirror that fails to write invalidates nothing. `.scratch/`
is already gitignored; that stays unchanged, and checkpoints are never committed.

### 3. Live repository state is the reconciliation source

Git, remote refs, PR state, CI state and repository contents are read live at every recovery. A checkpoint's
GitHub fields are history and are never trusted as current. The protocol names the four classes a recovery can
land in — RESUME, RECONCILE, REBASELINE, STOP_UNPROVABLE — so that "I could not prove it" cannot quietly
become "I assumed it".

### 4. A checkpoint carries evidence; it never carries authority

`authority_echo` records what the previous session was *permitted* to do, so a reader can judge whether the
work described was in scope when it happened. It is an echo, not a grant. Every new session starts READ_ONLY,
a SAME resume does not change that, and where a checkpoint and the current contract disagree the current
contract wins in both directions. The non-delegable prohibitions bind a recovery exactly as they bind
everything else.

### 5. Proof reuse is bounded by what the evidence was bound to

Base- and path-bound facts survive while the blobs they name are unchanged. Head-bound evidence — gates,
reviews, repairs, CI — is reusable only at the same applicable HEAD, and a final gate at a different HEAD is
not a final gate for the new HEAD. GitHub facts are always refreshed. Missing evidence invalidates that entry,
not the whole checkpoint. Re-running everything is explicitly *not* the recovery strategy: it is as wrong as
trusting a stale gate, and it teaches the next session that checkpoints are worthless.

### 6. The schema is a validator-controlled governance surface

`scripts/validate-agent-governance.py` gains one check, `session-recovery-doc`, that verifies the protocol
document mechanically: the schema fence selected by its `deep_checkpoint:` key, the `deep-checkpoint/v1`
contract id, every required field including the nested `authority_echo` and `publication` members, the
new-session READ_ONLY invariant, the checkpoint-never-authority invariant, the non-delegable never-list, the
redaction and untrusted-text rules, the SAME form, all four recovery classifications, and the proof-reuse
rules. Prose alone could not stop any of these regressing; the same reasoning that put the authority chain and
the contract schema under mechanical checks applies here.

## Rejected alternatives

- **`/tmp`-only continuity.** The mechanism `handoff` defaults to when no workspace scratch directory exists.
  Machine-local, world-readable on a shared host, and gone with the container. Rejected for the reason the
  vendored patch already gives.
- **`.scratch/`-only continuity.** Better than `/tmp` — gitignored, inside the workspace — but it still dies
  with the workspace, which is three of the five interruption classes. Kept as a mirror, rejected as the
  canonical carrier.
- **Tracked checkpoint commits.** Durable, but it puts session bookkeeping into the project's history, needs
  write authority precisely when a session may have none, creates merge conflicts between concurrent tasks,
  and would make every checkpoint a reviewable diff. Rejected.
- **PR or Issue bodies as canonical storage.** Durable and shared, but it requires standing GitHub write
  authority for a bookkeeping operation, is unavailable to a READ_ONLY session, and mixes session state into
  a surface reviewers read as project communication. Rejected.
- **A new first-party skill.** A skill would need its own entry in `docs/agents/project-skills-manifest.json`,
  its own review status, eval evidence and blob hashes — a whole ownership class of machinery for what is one
  protocol document plus one validator check. The manifest deliberately ships empty; this is not the thing to
  open it with. Rejected.
- **Patching `handoff` or `ce-handoff`.** Both are vendored under a minimal-local-patching policy, and neither
  is *broken*: they do session handoff well. The gap is a different granularity, not a defect in theirs.
  Patching them would create a local fork of upstream behaviour for a purpose upstream never claimed.
  Rejected; the protocol composes with them instead.

## Consequences

- Fresh-session transfer costs the owner one copy-paste of a block that was already printed. That is the
  price of the only carrier that crosses a workspace boundary, and it is paid only when a boundary is actually
  crossed.
- No new storage subsystem, no new dependency, no new tracked state file, and no standing GitHub write
  requirement. The protocol is one document, one ADR, three pointers and one validator check.
- The schema and its invariants become a governance surface: changing them is a reviewed change that must keep
  `session-recovery-doc` green, exactly like `governance-v3-contract-schema`.
- Continuity can now carry *evidence* between sessions without carrying *authority*, which is the property
  that made the gap worth closing rather than living with.
- Honest limits stay stated: `.scratch/` is not durable, a checkpoint is not a backup, and a recovery that
  cannot prove its state stops rather than guesses.

## Links

- `docs/agents/session-recovery.md` — the normative protocol and the `deep-checkpoint/v1` schema
- `docs/agents/operation-modes.md` — session boundary; a checkpoint never restores a mode
- `docs/agents/task-contract.md` — the authority envelope a recovery must re-establish
- `docs/agents/quality-gates.md` — final-gate evidence, and why a gate does not travel between HEADs
- `docs/agents/skills-routing.md` — routing, and composition with `handoff` / `ce-handoff`
- `docs/adr/0002-governance-v3-skill-native-orchestration.md` — the authority model this builds on
- `scripts/validate-agent-governance.py` — the `session-recovery-doc` check
