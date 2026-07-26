# Issue tracker: GitHub

Issues and PRDs for this repo live as GitHub issues on `KraineOpasen/bukerov-twitch-miner-go`. Use the `gh` CLI
for all operations. Infer the repo from `git remote -v` — `gh` does this automatically inside this clone.

**Default posture: read-only.** Reading issues/PRs is always allowed. Every operation below that creates,
edits, labels, comments on, or closes an issue or PR **requires an explicit task contract** authorizing tracker
mutations (see `docs/agents/task-contract.md`) — do not run it speculatively or "to be helpful".

## Conventions

### Read-only (always allowed)

- **Read an issue**: `gh issue view <number> --comments`.
- **List issues**: `gh issue list --state open --json number,title,body,labels,comments` with `--label`/
  `--state` filters as needed.
- **Read a PR**: `gh pr view <number> --comments` and `gh pr diff <number>` for the diff.
- **List PRs**: `gh pr list --state open --json number,title,body,labels,author,authorAssociation,comments`.

### Mutating (requires an explicit task contract)

- **Create an issue**: `gh issue create --title "..." --body "..."` (heredoc for multi-line bodies).
- **Comment**: `gh issue comment <number> --body "..."` / `gh pr comment <number> --body "..."`.
- **Apply / remove labels**: `gh issue edit <number> --add-label "..."` / `--remove-label "..."`.
- **Close**: `gh issue close <number> --comment "..."` / `gh pr close <number>`.
- Anything under "Wayfinding operations" below.

GitHub shares one number space across issues and PRs, so a bare `#42` may be either — resolve with
`gh pr view 42`, falling back to `gh issue view 42`.

## Pull requests as a triage surface

**PRs as a request surface: off.** `/triage` does not pull external PRs into its buckets. This can be flipped
on later by an explicit user decision, documented here — it is not on by default.

## When a skill says "publish to the issue tracker"

Create a GitHub issue — but only under an explicit task contract authorizing the mutation. Absent one, produce
the content as a local draft (in the answer, or a file under an allowed docs/scratch path) and say so.

## When a skill says "fetch the relevant ticket"

Run `gh issue view <number> --comments` (read-only, always allowed).

## Wayfinding operations

Used by `/wayfinder`, and gated the same as any other tracker mutation: default to local files (`.scratch/` or
`/tmp`); only touch the real tracker under an explicit contract.

- **Map**: a single issue labelled `wayfinder:map`, holding the Notes / Decisions-so-far / Fog body —
  `gh issue create --label wayfinder:map`.
- **Child ticket**: linked to the map as a GitHub sub-issue, or via a task-list line `Part of #<map>` where
  sub-issues aren't enabled. Labels: `wayfinder:<type>` (`research`/`prototype`/`grilling`/`task`).
- **Blocking**: GitHub's native issue dependencies —
  `gh api --method POST repos/<owner>/<repo>/issues/<child>/dependencies/blocked_by -F issue_id=<blocker-db-id>`.
  **This exact call is unconditionally denied by `.claude/hooks/governance-policy.py`** (it matches the
  mutating-`gh api`-method rule) — no task contract can turn it back on, since the hook doesn't read the
  contract at all. Enabling native issue-dependency wiring from an agent session would require a human editing
  the hook's policy directly (outside Claude Code, since `.claude/hooks/**` is itself edit-denied — see
  `CLAUDE.md`'s governance section). Until then, fall back to a `Blocked by: #<n>` line in the child ticket's
  body, written under the same task-contract gate as any other tracker mutation.
- **Frontier query** (read-only): list the map's open children, drop any with an open blocker or an assignee.
- **Claim**: `gh issue edit <n> --add-assignee @me` (mutating; contract required).
- **Resolve**: comment the answer, close the ticket, append a context pointer to the map (all mutating).
