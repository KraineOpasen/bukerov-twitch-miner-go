# Operation modes

Four **capability ceilings** on what an agent session may do to this repo. The active mode is set by the task
contract (see `docs/agents/task-contract.md`); absent a contract, the mode is always `READ_ONLY`. A contract
can only grant a mode — it can never grant capabilities the authority chain in `CLAUDE.md` reserves for a
direct user command (merge, release, deploy).

Modes bound **reach**, never workflow. A mode says which operations are available to the session; it does not
prescribe how a skill organizes its agents, lanes, reviewers, or repair loops — that is skill-native and lives
in `docs/agents/agent-orchestration.md`. Every subagent inherits the session's mode; none may exceed it.

## READ_ONLY (default)

- **Allowed**: read files, search, run local read-only commands (`git status`/`diff`/`log`/`show`, `go build`,
  `go test`, `go vet`, `docker build`, `docker compose config`, `gh ... view`/`list`), produce reports or
  drafts in the answer or an allowed scratch/docs path.
- **Forbidden**: `Edit`/`Write`/`NotebookEdit` on tracked files, `git commit`, `git push`, any tracker mutation
  (issue/PR create, comment, label, close, assign).

## PROTOTYPE

- **Allowed**: throwaway code in `/tmp` or a disposable worktree to answer a design question (see the vendored
  `prototype` skill).
- **Forbidden**: treating the prototype as the production implementation; committing or pushing it without a
  separate explicit contract addendum; branch creation on the primary checkout.

## CHANGE

- **Allowed**: `Edit`/`Write` on the task branch named in the contract; `git commit` on that branch.
- **Forbidden**: `git push`; edits on `main`/`master`; merge, release, deploy, or any GitHub settings/secrets
  mutation — always out of scope regardless of contract (see `CLAUDE.md` non-delegable prohibitions).

## PUBLISH_DRAFT

- **Allowed**: `git push` to the task's non-main branch; opening a **Draft** PR, only when the contract's
  capabilities explicitly authorize it.
- **Forbidden**: marking the PR ready for review, enabling auto-merge, merging, releasing, or deploying — those
  require a separate direct user command, outside this policy's autonomous execution.

## Transitions

Modes only escalate in order — READ_ONLY -> PROTOTYPE -> CHANGE -> PUBLISH_DRAFT — and only as far as the
active contract's granted mode. Any expiry trigger drops the session straight back to READ_ONLY, regardless of
which mode it was in.

## Expiry triggers (force revert to READ_ONLY)

These are **integrity and authority** failures — the contract's premises stopped holding, or something was
attempted outside them:

- Repo switch (a tool targets a different repository than the contract names)
- Unexpected branch (current branch != contract branch)
- Main drift (main/master's HEAD SHA has moved since the contract's base SHA)
- Dirty worktree the contract didn't expect
- A competing PR appears on the task branch
- An operation outside granted authority, attempted by any agent at any delegation depth
- Irreconcilable scope expansion
- Environment corruption, or state that can no longer be proven
- A **final** gate that cannot be made to pass after the skill's repair strategy is exhausted (see
  `docs/agents/quality-gates.md`)
- Session end

**Not an expiry trigger:** ordinary development feedback. A red test, a failing build, a review finding, a
surviving mutant, an expected TDD red state — these are diagnosed and repaired inside the active task. Only an
exhausted repair strategy on a final gate expires the contract.

## Drift handling

Before acting on a mode's permissions, verify the facts the contract assumed still hold (repo, branch, base
SHA, PR/CI state — see `docs/agents/task-contract.md`'s re-check points). On any detected drift, stop, report
what changed with exact evidence, and treat the contract as expired until re-verified.

## Session boundary

Mode and contract state exist only for the current live session. A new session always starts at READ_ONLY,
regardless of what a prior session did or claimed. Background subagents may run only inside an active session
— never report work as continuing or completing after the session that spawned it has ended.

Continuity is not an exception to that rule, it is built around it. A DEEP task interrupted mid-flight may
carry its *evidence* forward through the checkpoint protocol in `docs/agents/session-recovery.md` — what was
proven, at which SHA, which gate last passed, what remains. A checkpoint never restores a mode and never
restores authority: the resumed session still starts READ_ONLY and still needs a current task contract before
it may mutate anything.
