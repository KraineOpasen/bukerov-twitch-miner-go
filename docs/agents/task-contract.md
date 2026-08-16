# Task contract

An explicit, machine-readable **authority envelope** for the current session. No contract means READ_ONLY
(see `docs/agents/operation-modes.md`) — a contract is what lets a session do more than read.

A contract states what the session may *reach*: which repo, which branch, which paths, which mutations, and
where publication stops. It is **not** an orchestration recipe. How the engineering gets done — which agents
run, in what order, writing what — belongs to the invoked audited skill (see
`docs/agents/agent-orchestration.md`). A contract does not need to enumerate agents, roles, lanes, or caps,
and a prompt does not need to state an agent topology.

## Schema

Every field is optional except `mode`, `repository`, `base_branch`, `base_sha`, `task_branch`, and
`authorized_by` — omitting an optional field means "not granted," not "unrestricted."

```yaml
task_contract:
  task_id: "<short slug>"
  mode: READ_ONLY | PROTOTYPE | CHANGE | PUBLISH_DRAFT
  repository: "KraineOpasen/bukerov-twitch-miner-go"
  base_branch: "main"
  base_sha: "<HEAD sha the task started from>"
  task_branch: "<task branch name>"           # never main/master
  allowed_paths: []                           # globs the session may touch; empty = repo-wide per mode's normal scope
  allowed_file_operations: []                 # subset of: read, edit, write, delete
  allowed_git_operations: []                  # subset of: branch, commit, push, rebase, merge_local
  allowed_github_operations: []               # subset of: issue_read, issue_write, pr_read, pr_draft, pr_comment
  quality_tier: Q0 | Q1 | Q2 | Q3              # see docs/agents/quality-gates.md
  orchestration: skill_native | main_context_only   # optional; absent => skill_native
  agent_cap: <int>                             # optional resource ceiling; absent => no cap
  max_concurrency: <int>                       # optional resource ceiling; absent => no cap
  capabilities: []                             # named extra grants, e.g. write_research_docs, tracker_mutations
  forbidden: []                                # explicit call-outs beyond the always-forbidden list below
  authorized_by: "<human, explicitly>"
  # Never present, never contract-grantable, regardless of any field above: merge, auto_merge,
  # release, deploy, production_access, workflow_trigger, github_settings, github_secrets,
  # force_push, push_to_main — those require a separate direct user command outside this policy,
  # and even then are not executed autonomously.
```

## Field notes

- **`mode`** is the capability ceiling from `docs/agents/operation-modes.md`; `allowed_git_operations`/
  `allowed_github_operations` narrow *within* that ceiling, they never widen past it (a `CHANGE`-mode contract
  listing `push` in `allowed_git_operations` still can't push — `push` only exists in `PUBLISH_DRAFT`).
- **`base_branch`**/**`base_sha`** pin what "drift" means for this contract (see operation-modes' expiry
  triggers); **`task_branch`** is the one branch `allowed_git_operations` apply to.
- **`allowed_paths`** scopes `allowed_file_operations` — a contract can grant `edit` narrowly (e.g. only
  `docs/**`) without granting it repo-wide. This binds every agent at every delegation depth.
- **`quality_tier`** states which final gate this task must clear before it's considered done — not every task
  needs `Q3`; a documentation-only change may cap at `Q1`.
- **`orchestration`** is the only orchestration field, and it is an **opt-out**. Absent, orchestration is
  `skill_native`: invoking an audited skill authorizes the agent topology that skill documents. Set
  `main_context_only` for the rare task that must run with no subagents at all — a transitional governance
  change, a single serialized edit, a debugging session where fan-out would obscure the signal.
- **`agent_cap`** / **`max_concurrency`** are **optional resource ceilings**, not orchestration policy. They
  are no longer mandatory and have no default: absent from the contract there is no cap, and the skill's own
  design governs fan-out. Set them only when a task genuinely needs a resource bound. Where a vendored skill's
  local patch text or a patch-ledger row still references a cap — "the task contract's `agent_cap`", "the task
  contract's concurrency cap", "the session's agent budget", "its agent cap", or any other phrasing — read it
  as *"respect a cap if the contract sets one"*, never as an assertion that a cap always exists (see
  `docs/agents/agent-orchestration.md`).
- **`forbidden`** is for task-specific call-outs on top of the always-forbidden list (e.g. a task might add
  `forbidden: [schema_migration]` if this particular change must not touch `internal/database`'s migrations)
  — it narrows further, it never removes anything from the always-forbidden list.

## What it means in practice

A contract names one repo, one branch, one base SHA, and a ceiling on what the session may reach. It is scoped
— granting `CHANGE` doesn't imply `PUBLISH_DRAFT`; granting `tracker_mutations` doesn't imply `merge`. Read the
contract narrowly: if a capability isn't listed, treat it as not granted.

A contract can never authorize merge, auto-merge, release/tag, deploy to any runtime environment, workflow
trigger/rerun, or GitHub settings/secrets changes — those always require a separate, direct user command, and
even then this policy does not execute them autonomously. **The owner performs merges.**

## Authority inheritance

Every subagent, at any depth, inherits **this same envelope**. A child may be handed a narrower one; it may
never widen mode, repo, allowed paths, or publication authority, and may never invent authority the contract
does not carry. Authority does not accumulate through delegation — see
`docs/agents/agent-orchestration.md`.

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
noise without adding safety. A failing test or a review finding between checkpoints is development feedback,
not drift; it does not trigger a re-preflight and does not end the contract (see
`docs/agents/quality-gates.md`).
