# Operation modes

Four modes gate what an agent session may do to this repo — the repo-native elaboration of
`GOVERNANCE_V3.md` §4. The active mode is set by the task contract (see `docs/agents/task-contract.md`);
absent a contract, the mode is always `READ_ONLY`. A contract can only grant a mode — it can never grant
the owner-gated actions `GOVERNANCE_V3.md` §4 reserves for a separate, direct owner command (marking a PR
ready for review, merge/auto-merge, tag/release/deploy, workflow trigger/rerun, repo settings/secrets).

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
- **Forbidden**: `git push`; edits on protected branches (`main`/`master`/`release/*`); merge, release,
  deploy, or any GitHub settings/secrets mutation — always out of scope regardless of contract (see
  `GOVERNANCE_V3.md` §4).

## PUBLISH_DRAFT

- **Allowed**: non-force `git push` of the task branch (never a protected branch); opening exactly one
  **Draft** PR, only when the contract's capabilities explicitly authorize it.
- **Forbidden**: marking the PR ready for review, enabling auto-merge, merging, releasing, or deploying — those
  require a separate direct user command, outside this policy's autonomous execution.

## Transitions

Modes only escalate in order — READ_ONLY -> PROTOTYPE -> CHANGE -> PUBLISH_DRAFT — and only as far as the
active contract's granted mode. Any expiry trigger drops the session straight back to READ_ONLY, regardless of
which mode it was in.

## Expiry triggers (force revert to READ_ONLY)

- Repo switch (a tool targets a different repository than the contract names)
- Unexpected branch (current branch != contract branch)
- Base drift (the contract's base branch HEAD SHA has moved since the contract's `base_sha`)
- Dirty worktree the contract didn't expect
- A competing PR appears on the task branch
- A repair strategy exhausted without an honestly passing final gate (an ordinary red test, build error, or
  review finding is development feedback to repair inside the task, not an expiry trigger — see
  `GOVERNANCE_V3.md` §5 and `docs/agents/quality-gates.md`)
- Session end

## Drift handling

Before acting on a mode's permissions, verify the facts the contract assumed still hold (repo, branch, base
SHA, PR/CI state — see `docs/agents/task-contract.md`'s re-check points). On any detected drift, stop, report
what changed, and treat the contract as expired until re-verified.

## Session boundary

Mode and contract state exist only for the current live session. A new session always starts at READ_ONLY,
regardless of what a prior session did or claimed. Background subagents may run only inside an active session
— never report work as continuing or completing after the session that spawned it has ended.
