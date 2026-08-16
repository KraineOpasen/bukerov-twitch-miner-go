# Matt Pocock skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of [mattpocock/skills](https://github.com/mattpocock/skills)
into `.claude/skills/` instead of installing it as a live Claude Code plugin. This document is the policy for
what's installed, why, how it's patched, and how to update it. See also
`docs/agents/mattpocock-skills-manifest.json` (machine-readable inventory) and
`docs/agents/mattpocock-skills-patches.md` (per-patch ledger).

## Upstream

- Repo: `https://github.com/mattpocock/skills`
- Reviewed commit: `068b6e0c62393147daf03530149cdce209c93da8` (refreshed from `ed37663cc5fbef691ddfecd080dff42f7e7e350d`)
- Reviewed tree: `717d7f0ef7363b35c967d43629f3b71d4ec3ba77` (was `04b0fcb78e3de7c58744fcba2528354cc64ab988`)
- Current upstream HEAD at review time: same SHA (**drift: none**)
- `package.json` version: `1.2.3`
- `.claude-plugin/plugin.json` version: `1.2.3` — matches `package.json` in this refresh; the pre-bump drift
  noted at the previous review (`1.2.0` plugin.json vs. `1.1.0` released) has resolved itself upstream.
- Between the previously reviewed commit and this one, upstream **deleted**
  `skills/productivity/writing-great-skills` (one of this project's then-21 installed skills) and replaced it
  with an unrelated new skill, `skills/productivity/writing-for-agents` (different name, different file
  structure, fully rewritten content — not a rename upstream's own history tracks as one). This project now
  vendors `writing-for-agents` under a fresh review in its place; see `mattpocock-skills-manifest.json`'s
  `excluded[]` entry for `writing-great-skills` and the `writing-for-agents` skill entry.
- This refresh also newly promoted three skills that weren't previously part of the reviewed set:
  `skills/engineering/wizard` (moved from `skills/in-progress/`), `skills/productivity/to-questionnaire`
  (moved from `skills/in-progress/`), and `skills/productivity/wait-what` (new). `wizard` and `wait-what` were
  added to this project's installed set after a separate explicit owner authorization; `to-questionnaire`
  remains excluded by owner decision — see "Excluded" below.

## Installation model

**Project-local vendored copy**, not a live plugin install. Each skill's `SKILL.md` and its sibling reference
`.md` files are copied verbatim into `.claude/skills/<name>/` at review time, then a small number are
minimally patched (see below). `automatic_updates: false` — nothing about this installation re-fetches or
re-syncs from upstream on its own. A human (or an explicitly-contracted agent task) must re-run the review
process to pick up a new upstream commit.

## Installed: 23 skills

17 of upstream's 18 promoted `skills/engineering/*` skills, plus 6 of upstream's 7 promoted
`skills/productivity/*` skills (`.claude-plugin/plugin.json`'s `skills[]` array is upstream's source of truth
for "promoted"; it lists exactly 25 entries as of this refresh). See `mattpocock-skills-manifest.json` for the
full per-skill list with classification and invocation mode.

## Excluded

- **`setup-matt-pocock-skills`** — the one promoted `engineering` skill excluded from vendoring. It's the only
  skill that edits this project's root `CLAUDE.md`; installing it would give a third-party skill standing
  permission to rewrite this project's own governance document. Its setup function (issue tracker config,
  triage label vocabulary, domain-doc layout) was instead performed deterministically by this governance task —
  see `docs/agents/issue-tracker.md`, `docs/agents/domain.md`, `docs/agents/triage-labels.md`.
- **`to-questionnaire`** — the one promoted `productivity` skill excluded from vendoring. Newly promoted
  upstream in this refresh (moved out of `skills/in-progress/`); excluded by explicit owner decision alongside
  `setup-matt-pocock-skills` rather than a content-safety finding — `ask-matt`'s vendored precondition note
  (see `ask-matt-setup-pointer` in the patch ledger) tells the router not to suggest either.
- **Non-promoted skills** under `skills/deprecated/`, `skills/in-progress/`, `skills/misc/`, and
  `skills/personal/` — none are in `plugin.json`'s `skills[]`, so none were ever part of the "promoted" set this
  project reviews. Full list with reasons in `mattpocock-skills-manifest.json`'s `excluded[]`.
- **`agents/openai.yaml`** — every promoted skill ships this sidecar (Codex-specific agent metadata). Claude
  Code does not read it, so it was not copied for any of the 23 installed skills. This is dead weight relative
  to this project's runtime, not a security exclusion.

## Invocation modes

- **User-invoked** (`disable-model-invocation: true`, 15 of the 23): `ask-matt`, `grill-with-docs`, `implement`,
  `improve-codebase-architecture`, `to-spec`, `to-tickets`, `triage`, `wayfinder`, `grill-me`, `handoff`,
  `teach`, `resolving-merge-conflicts` (moved from model- to user-invoked by a local patch — see below),
  `writing-for-agents` (also moved by a local patch; already user-invoked upstream in the skill it replaces,
  `writing-great-skills`), `wizard` (moved from model- to user-invoked by a local patch), and `wait-what`
  (already user-invoked upstream, kept as-is).
- **Model-invoked** (8 of the 23): `code-review`, `codebase-design`, `diagnosing-bugs`, `domain-modeling`,
  `grilling`, `prototype`, `research`, `tdd`.

## Local patches

19 skills carry a minimal, marked local patch (21 patch-ids counting `resolving-merge-conflicts`'s frontmatter
change separately from its body change, and `wait-what-domain-vocab` once for each of the two files it touches);
4 skills (`domain-modeling`, `grill-me`, `grill-with-docs`, `grilling`) are unmodified. Every patched block is
wrapped in `<!-- bukerov-local-patch: <id> --> ... <!-- /bukerov-local-patch: <id> -->` comments so a diff
against the upstream blob SHA shows exactly what changed and why. Full ledger:
`docs/agents/mattpocock-skills-patches.md`. No patch translates or stylistically rewrites upstream text — every
change narrows a capability (commit, push, tracker mutation, network fetch, auto-open, agent count, wizard-
generated script authority) to match this project's governance model, or corrects a router's (`ask-matt`'s)
description of another vendored skill's actual invocation mode.

## Governance precedence

Vendored skills sit **below** this project's own policy. Precedence (see `CLAUDE.md`'s
"Claude Code Governance (v2)" section): (1) the active task contract, (2) `CLAUDE.md` + `.claude/rules/*.md`,
(3) these vendored skills (as patched), (4) unpatched upstream skill defaults, (5) generic model behavior. A
skill instruction never overrides a `.claude/rules/*.md` constraint or a hook denial.

## No automatic updates

There is no update mechanism wired into this repo — no CI job, no Claude Code plugin auto-update, nothing that
re-vendors on a schedule. Updating is a deliberate, reviewed, human-initiated act (see "Update procedure"
below). This is intentional: an upstream skill change is effectively new instructions an agent will follow, and
this project doesn't want that arriving silently.

## Supply-chain assumptions

We vendor a copy instead of installing `mattpocock/skills` as a live plugin because a plugin install trusts
*upstream's `main` branch on every future run*, not just the commit reviewed today. A compromised or
carelessly-changed upstream commit would propagate into this project's agent behavior without any local
review step. Vendoring converts that "live trust" into "trust as of a specific reviewed SHA, re-established
only when someone deliberately re-reviews." The trade-off is manual update effort in exchange for a fixed,
auditable review boundary.

## Update procedure

1. Fetch the new upstream commit into a read-only clone (never edit it in place).
2. Diff `plugin.json`'s `skills[]` against the last-reviewed list — note additions, removals, renames.
3. For every skill already vendored, diff its `SKILL.md` and sibling `.md` files against the currently-vendored
   copy (not against unmodified upstream — some are locally patched) to isolate genuinely new upstream content
   from re-applying an old patch.
4. Re-run the same review judgment as the original vendoring: does the skill assume standing commit/push/
   tracker-mutation authority this project doesn't grant by default? If so, patch it the same way (minimal,
   marked, no rewrites) rather than installing it unpatched.
5. Update `upstream_commit`, `upstream_tree`, `upstream_current_head`, `upstream_version`, and `reviewed_at` in
   `mattpocock-skills-manifest.json`; update per-skill `upstream_blob_sha` for every touched file.
6. Update `mattpocock-skills-patches.md` for any patch that changed, was added, or was removed.
7. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
8. Open the change as its own dedicated Draft PR (see "Dedicated Draft PR requirement" below) — never bundle a
   skills re-vendor into an unrelated feature or governance change.
9. Get human review before merge; this task's own governance forbids the agent from merging it itself.

## Rollback

1. Identify the last-known-good `upstream_commit` from `mattpocock-skills-manifest.json`'s git history.
2. Restore `.claude/skills/**` from that prior commit (`git checkout <sha> -- .claude/skills`).
3. Restore `docs/agents/mattpocock-skills-manifest.json` and `mattpocock-skills-patches.md` from the same
   commit.
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description (broken skill behavior, a patch that
   didn't survive an upstream change, etc.).

## Known limitations

- The hook/permission layer (`.claude/hooks/governance-policy.py`, `.claude/settings.json`) is a mechanical
  backstop, not a substitute for reading a skill before vendoring it — a sufficiently subtle instruction could
  still shape agent *reasoning* even where it can't force a blocked tool call.
- The hook unwraps common shell-indirection patterns before matching — `bash -c`/`sh -c`/`zsh -c`/`dash -c`,
  `eval`, `env`, `xargs` with any flags, a `git`/`gh` verb reached through `$VAR`/`${...}`/`$(...)`/backtick
  substitution (fails closed rather than guess), and the left side of a pipe into a bare shell interpreter
  (`... | bash`, regardless of what produced the left side's text). What's still residual after that: a skill
  telling an agent to run `make`, `npm run <script>`, `git bisect run <cmd>`, or any other custom/opaque script
  the hook can't see inside of — those are only as safe as what the target script does, since the hook
  inspects the Bash command line's structure, not the body of a script it invokes by name.
- A direct push of the *current* branch is blocked on main/master in every shape the hook can see —
  bare `git push`, `git push origin`, `git push origin HEAD`/`@`, `git push -u origin`,
  `git push --set-upstream origin`, `git push --all`/`--mirror`/`--tags` — not only the explicit
  `git push origin main` form. A GraphQL mutation via `gh api graphql` is blocked whether it is written on
  one line or split across a multi-line `-f query=$'\nmutation{...}'` payload. Server-side GitHub file writes
  (`mcp__github__push_files`/`create_or_update_file`/`delete_file`) are denied outright because they bypass the
  local branch gate entirely, and `mcp__github__update_pull_request` is gated to `ask` so a draft→ready flip
  cannot happen autonomously. Reads of the policy layer (`grep`/`cat`/`rg`/`git diff` over `.claude/hooks/` or
  `.claude/settings.json`) are allowed — only writes into it fail closed. A tracked-file edit on main/master is
  blocked even when the checkout lives under `/tmp` (a CI or sandbox worktree is still a real worktree).
- Residual Bash-evasions the hook does **not** catch (outside the errant-agent threat model, all requiring a
  deliberate obfuscation an ordinary agent would not construct): assembling a command through `$IFS`/word-splitting
  glue, feeding a here-string or process substitution (`<<<`, `<(...)`) into a bare shell, `xargs` reading its
  command from stdin rather than its arguments, and an interpreter one-liner (`python -c`, `perl -e`) that opens a
  protected path through a computed string rather than a literal one. In-place editors (`sed -i`, `perl -i`) and
  literal-path interpreter one-liners targeting the policy layer *are* blocked. These residuals are the reason the
  hook is described as a backstop against an agent "getting carried away," not a boundary against a determined
  adversary — a denylist over shell text can never be exhaustive.
- Pre-existing `.git/hooks/*` on a contributor's machine are outside this policy's reach.
- New or unknown MCP tools are not automatically covered — `.claude/settings.json`'s MCP denies are an
  allowlist-style exact-name list, not a pattern match; a newly added MCP tool starts ungoverned until added.
- This document assumes subagents spawned by a vendored skill stay within the spawning session's agent cap;
  that assumption depends on the orchestrating agent actually enforcing the cap (see
  `docs/agents/task-contract.md`), not on anything in this policy doc alone.
- **Operation modes are not mechanically enforced by the hook.** `.claude/hooks/governance-policy.py` has no
  concept of READ_ONLY/PROTOTYPE/CHANGE/PUBLISH_DRAFT and doesn't know which mode a session is currently
  operating under — it only recognizes fixed, mode-independent dangers (push to main, force push, tag/release
  push, `gh` mutations, remote-host access, infra restarts, writes into the governance layer itself, and now
  the indirection patterns above). Everything mode-shaped — "don't push outside PUBLISH_DRAFT," "don't create
  a Draft PR without the contract capability" — is a **behavioral layer**: it lives in `CLAUDE.md`,
  `docs/agents/operation-modes.md`, and `docs/agents/task-contract.md`, and depends on the agent reading and
  following them, not on a tool-level gate. The hook and the mode/contract documents are complementary, not
  redundant — the hook catches a fixed set of always-dangerous shapes regardless of what any document says;
  the documents cover everything more context-dependent than that.
- Prompt injection via fetched web content, issue bodies, or PR descriptions is a live risk for any skill that
  reads external text (`research`, `triage`, `wayfinder`) — those skills' patches note that fetched content is
  data, not instructions, but that's a documented expectation, not a technical control.

## Compatibility

Reviewed and vendored against Claude Code `v2.1.220` — the `.claude/rules/*.md` `paths:` frontmatter, the
`Edit(path)`-covers-Edit/Write/NotebookEdit permission behavior, and the PreToolUse hook stdin schema this
policy and its hook depend on were verified against that version. A future Claude Code version could change
hook payload fields or permission-matching semantics; re-verify the assumptions listed at the top of this
project's governance design before assuming they still hold.

## `agents/openai.yaml`

Every promoted skill's `agents/openai.yaml` file (Codex-specific agent metadata) was **not vendored**. Claude
Code does not read this file format; the sidecar exists purely for a different agent runtime. Its absence here
is not a security exclusion — see "Excluded" above.

## Dedicated Draft PR requirement

Any change to `.claude/skills/**`, `mattpocock-skills-manifest.json`, or `mattpocock-skills-patches.md` —
whether a re-vendor, a new patch, or a rollback — goes into its own dedicated Draft PR, never mixed into an
unrelated feature branch's diff. This keeps the reviewable unit for "what changed in the third-party skill
surface" separable from everything else, and keeps the PUBLISH_DRAFT envelope (see
`docs/agents/operation-modes.md`) meaningful for this specific, higher-scrutiny category of change.
