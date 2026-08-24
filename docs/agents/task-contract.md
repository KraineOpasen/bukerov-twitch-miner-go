# Task contract

An explicit, machine-readable grant of capability for the current session. No contract means READ_ONLY (see
`docs/agents/operation-modes.md`) — a contract is what lets a session do more than read.

## Schema

The full envelope a task contract may specify — the machine-readable form of the contract minimum in
`GOVERNANCE_V3.md` §3. Every field is optional except `mode`, `repository`, `base_branch`, `base_sha`,
`task_branch`, and `authorized_by` — omitting an optional field means "not granted," not "unrestricted."

```yaml
task_contract:
  task_id: "<short slug>"
  mode: READ_ONLY | PROTOTYPE | CHANGE | PUBLISH_DRAFT
  repository: "KraineOpasen/bukerov-twitch-miner-go"
  base_branch: "release/0.1"                  # the live active stable line — GOVERNANCE_V3.md §2
  base_sha: "<HEAD sha the task started from>"
  task_branch: "<task branch name>"           # never a protected branch (main/master/release/*)
  allowed_paths: []                           # globs the session may touch; empty = repo-wide per mode's normal scope
  allowed_file_operations: []                 # subset of: read, edit, write, delete
  allowed_git_operations: []                  # subset of: branch, commit, push, rebase, merge_local
  allowed_github_operations: []               # subset of: issue_read, issue_write, pr_read, pr_draft, pr_comment
  quality_tier: Q0 | Q1 | Q2 | Q3              # see docs/agents/quality-gates.md
  agent_cap: <int>                             # max subagents this task may have alive at once
  max_concurrency: <int>                       # max subagents spawned in parallel in a single batch
  orchestration: skill_native | main_context_only   # default skill_native — GOVERNANCE_V3.md §10
  capabilities: []                             # named extra grants, e.g. write_research_docs, tracker_mutations
  forbidden: []                                # explicit call-outs beyond the always-forbidden list below
  authorized_by: "<human, explicitly>"
  # Never present, never contract-grantable, regardless of any field above: merge, auto_merge,
  # release, deploy, production_access, workflow_trigger, github_settings, github_secrets — those
  # require a separate direct user command outside this policy, and even then are not executed
  # autonomously. Force push and any direct push to a protected branch (main/master/release/*)
  # are always forbidden, with or without such a command (GOVERNANCE_V3.md §4).
```

## Field notes

- **`mode`** is the ceiling from `docs/agents/operation-modes.md`; `allowed_git_operations`/
  `allowed_github_operations` narrow *within* that ceiling, they never widen past it (a `CHANGE`-mode contract
  listing `push` in `allowed_git_operations` still can't push — `push` only exists in `PUBLISH_DRAFT`).
- **`base_branch`**/**`base_sha`** pin what "drift" means for this contract (see operation-modes' expiry
  triggers); **`task_branch`** is the one branch `allowed_git_operations` apply to.
- **`allowed_paths`** scopes `allowed_file_operations` — a contract can grant `edit` narrowly (e.g. only
  `docs/**`) without granting it repo-wide.
- **`quality_tier`** states which gate this task must clear before it's considered done — not every task needs
  `Q3`; a documentation-only change may cap at `Q1`.
- **`orchestration`** defaults to `skill_native` (`GOVERNANCE_V3.md` §10): invoking an audited skill
  authorizes the agent topology that skill documents, and the writer invariant is **no uncontrolled
  competing writes** — deterministic ownership partitioning with reconciliation before the final gates —
  not "one writer". `main_context_only` is a rare, explicit owner/task opt-out, never the default; every
  child agent inherits the envelope (child ≤ parent at every delegation depth).
- **`forbidden`** is for task-specific call-outs on top of the always-forbidden list (e.g. a task might add
  `forbidden: [schema_migration]` if this particular change must not touch `internal/database`'s migrations)
  — it narrows further, it never removes anything from the always-forbidden list.

## What it means in practice

A contract names one repo, one branch, one base SHA, and a ceiling on what the session may do. It is scoped —
granting `CHANGE` doesn't imply `PUBLISH_DRAFT`; granting `tracker_mutations` doesn't imply `merge`. Read the
contract narrowly: if a capability isn't listed, treat it as not granted.

A contract can never authorize merge, auto-merge, release/tag, deploy/production/TrueNAS access, workflow
trigger/rerun, or GitHub settings/secrets changes — those always require a separate, direct user command, and
even then this policy does not execute them autonomously.

## Mandatory re-check points

Re-verify the contract's assumptions (repo, branch, base SHA, dirty-worktree state, competing PR, CI state)
at exactly these points — not before every single tool call:

1. Before creating or switching to the task branch.
2. Before the first production edit (first `Edit`/`Write` on a tracked file).
3. Before the first commit.
4. Before any push.
5. Before opening a Draft PR.
6. After any detected drift (see `docs/agents/operation-modes.md`'s expiry triggers).

**No full re-preflight before every `Edit`.** Once verified at a checkpoint, keep working through the next
checkpoint without re-running the whole verification sequence on every file touched — that would just add
noise without adding safety.
