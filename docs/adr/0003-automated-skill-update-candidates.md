# ADR-0003: Automated vendored-skill update candidates

- **Status**: Accepted
- **Date**: 2026-08-17

## Context

This project vendors 81 skills from six upstream providers into `.claude/skills/**`, pinned to an
exact commit per provider and recorded in `docs/agents/<provider>-skills-manifest.json`. ADR-0001
established why: a live plugin install trusts upstream's `main` on every future run, whereas
vendoring converts that into "trust as of a specific reviewed SHA, re-established only when
someone deliberately re-reviews".

That trade has a standing cost the earlier ADRs left to human diligence: **noticing that upstream
moved**. Nothing checked. A provider could sit months behind a security-relevant fix, or drift so
far that the eventual re-vendor became a rewrite rather than a review. The manifests also record
`automatic_updates: false` per provider, which correctly describes the policy but does nothing to
surface the drift that policy is deliberately not acting on.

At the same time, the obvious automation — a bot that bumps the pin and opens a PR — would destroy
the property the vendoring exists to create. If a machine can move a pin and the result looks like
any other reviewed change, then "reviewed at a specific SHA" has quietly become "whatever the bot
merged", and every downstream guarantee in `docs/agents/*-skills-policy.md` becomes decorative.

A sixth complication: `mattpocock` was still on the older skill-level manifest schema, recording
one hash per skill's `SKILL.md`. Of its 42 vendored files, 17 carried no recorded hash anywhere
and `provider-file-hashes` did not run for that provider at all — a gap its own policy documented
under "Known limitations" and named migration as the fix. No drift detector can be trusted over
files it has no baseline for.

## Decision

Automate **noticing** and **mechanical preparation**. Do not automate **review**, and make that
distinction mechanical rather than a matter of wording in a PR body.

### 1. The bot proposes candidates; it cannot establish trust

A scheduled workflow resolves each provider's reviewed branch to a concrete commit, proves it,
and — when the refresh needs no judgement call — opens one Draft PR per provider containing
refreshed bytes and regenerated provenance. The PR body opens with
`AUTOMATED UPDATE CANDIDATE — AUDIT REQUIRED — DO NOT MERGE YET.`

### 2. A candidate cannot masquerade as audited

Every candidate manifest carries an `automated_candidate` block, and
`scripts/validate-agent-governance.py` **fails** while it is present — on the key's presence, not
its value, so renaming the state does not help. `reviewed_at`/`reviewed_by` are left untouched
because they are true statements about the superseded pin; rewriting them would be the bot
asserting a review it did not perform. Clearing the state requires a human to read the diff,
record fresh review fields, and delete the block.

Relatedly, `scripts_audited` is **withdrawn** when a candidate changes a script's bytes. That
attestation means a human read those bytes end to end; the validator's usual "re-audit, not just
re-hash" diagnostic cannot fire when the bot is itself the rehash.

### 3. Refuse rather than guess

Ten conditions block a candidate outright — `unprovable`, `ancestry`, `skill-set`, `inventory`,
`licence`, `executable`, `authority`, `conflict`, `patch-map` and `closure` (the codes in
`analyze.BLOCK_ORDER`). A blocked provider gets one deduplicated issue and **no partial PR**. Each
condition marks a place where the right answer depends on reading something.

One calibration matters here: `executable` fires only on a file that became executable *between*
the pinned and target commits, because several providers legitimately ship `100755` scripts this
project vendors `100644` under a documented patch id — blocking on their mere presence would
refuse every update forever and train readers to wave blocks through.

`authority` is calibrated the other way, and §10 below is the decision of record: it **includes**
`description` and `when_to_use`, and is checked in both directions (BASE→THEIRS for what upstream
changed, OURS→merged for what we might lose). Do not read the `executable` narrowing as licence to
narrow `authority` too — the two are opposite calls, made for opposite reasons.

### 4. Determinism over convenience

The three-way merge is hand-written over `difflib` rather than delegated to `git merge-file`,
whose output depends on the installed git version. Bytes are never normalized; lines split on
`\n` only. Re-running over unchanged inputs produces a byte-identical manifest, which is what
makes "no drift → no diff" a mechanical property rather than a hope, and what lets issue bodies be
compared for deduplication instead of tracked in a state file.

### 5. Upstream is data, never executable input

Provider repositories are fetched bare and read through `git cat-file`; no working tree is ever
created, so upstream content never becomes a named, moded, executable artifact. No fetched script
is run, including to assess it. Inputs are allowlisted rather than sanitized.

### 6. Migrate `mattpocock` to the file-level schema first

All six providers now record one hash pair per file, and a new check
(`all-providers-file-level`) fails closed if a provider is ever added on the retired schema —
`file_level_providers()` silently skips such entries, so the old arrangement would have given a
new provider zero hash coverage while every hash check still reported PASS.

### 7. Separate the three "skill" surfaces

"Skills" names three different distribution mechanisms, and a design that blurs them claims
coverage it does not have. **A** repo-vendored project skills under `.claude/skills/**` are owned
and updated here. **B** native marketplace plugins live in a user cache outside this repository
and are *monitored, never mutated* — the schema and comparison logic ship ready, the inventory
ships empty, and nothing runs `claude plugin update` or installs Claude Code in CI. **C**
claude.ai custom ZIP skills have no documented programmatic upload, so they are documentation and
package output only.

Plugin version resolution mirrors Claude Code's documented precedence (`plugin.json` >
marketplace entry > source commit > unknown) rather than inventing one, and compares the source
commit independently — an unchanged version label over changed bytes is the dangerous direction
and no version-string comparison can see it.

### 8. An explicit state machine, with one state automation may create

`NO_DRIFT`, `DRIFT_DETECTED`, `PREPARED_AUDIT_REQUIRED`, `BLOCKED`, `AUDITED`. Automation creates
`PREPARED_AUDIT_REQUIRED` and nothing else; `AUDITED` is reached by *deleting* the candidate block
after a review, never by writing the word. Naming the states makes the prohibition checkable
instead of implicit.

### 9. Classify how upstream moved, not just that it did

Only a fast-forward from the reviewed commit may produce a candidate. Diverged, rewritten and
unreachable histories are blocked. A force-push that replaces reviewed history with different
content of the same shape is invisible to any check that only compares tree contents — every hash
would still validate — so the ancestry relation is the only thing that catches it. Refs resolve to
full 40-hex SHAs and are compared at full length; the default branch is read from the remote
rather than assumed, and nothing hardcodes `main`.

### 10. Trigger surface over authority surface

The frontmatter keys requiring manual audit now include `description` and `when_to_use`. This
reverses an earlier calibration that treated them as ordinary prose. They are what the model reads
to decide whether to invoke a skill at all, so an upstream rewording changes *when the skill
fires*; a checker cannot distinguish a clarifying reword from one that widens the trigger. The
cost — more updates need a human — is accepted deliberately.

### 11. Provenance is not behavioural equivalence

A candidate whose changed files can alter behaviour is marked `EVAL_REQUIRED`, with generated
old-vs-candidate instructions for a fresh session. Evals are never run in Actions: they cost model
time and need a clean session with the candidate loaded. Saying so explicitly is the honest
alternative to letting a green provenance run be mistaken for a behavioural guarantee.

### 12. New sibling skills are discovered, not adopted and not blocking

A new skill outside the installed selection opens its own deduplicated `DISCOVERY_REQUIRED` issue
and does **not** block the provider's other updates. Blocking would make every actively maintained
provider permanently un-updatable; auto-adopting would install unreviewed instructions. Neither is
acceptable, so it becomes a tracked human decision on its own schedule.

### 13. Borrow concepts, depend on nothing

Third-party skill updaters were reviewed as design evidence only — declarative desired state,
managed-file ownership, dry run, idempotent reconciliation, dual human/JSON reports, protecting
unknown files, no-op silence, a version compatibility matrix. Nothing was vendored, installed,
executed or copied; two of the reviewed repositories lacked a complete recognized root licence,
which independently rules out copying from them. The canonical state remains this repository's
manifests, ledgers and git tree, with **no hidden state file** able to override them.

### 14. The bot does not widen its own authority

It holds `GITHUB_TOKEN` only, with `contents: read` for checking and
`contents`/`pull-requests`/`issues` write only for publication. It never marks a PR ready, merges,
tags, releases, force-pushes, pushes to the default branch, or changes repository settings. Where
a repository setting is required — *Allow GitHub Actions to create and approve pull requests* — it
is detected and reported with the exact remediation, never worked around.

## Consequences

**Good.** Drift becomes visible the day it happens instead of whenever someone remembers to look.
The mechanical half of a re-vendor — hashes, modes, `locally_modified`, `patch_ids`, tree SHAs — is
done by tested code rather than by hand, which is where hand-maintained provenance actually goes
wrong. The 17-files-with-no-hash gap is closed. And the review boundary is now enforced by a
failing check rather than by a sentence in a policy document.

**Costs.** The blocked conditions are deliberately conservative, so releases that restructure a
skill's file set will refuse and require a manual re-vendor — the bot will be least helpful
exactly when upstream changes most. A candidate PR does not start CI on its own (GitHub's
`GITHUB_TOKEN` recursion guard), so its governance status has to be read from the workflow summary
or reproduced locally. The daily schedule adds a small standing CI cost, bounded by skipping the
clone entirely for providers that have not moved.

**Accepted risk.** A clean three-way merge can still be *semantically* wrong: prose that merges
without conflict can change what a skill instructs an agent to do. Nothing mechanical catches
that, which is precisely why the candidate state exists, why behaviour-affecting candidates are
marked `EVAL_REQUIRED`, and why the PR body tells the reviewer to read the content diff rather
than trust the verdict table.

**Known limitation.** `scripts/validate-agent-governance.py` is not wired into any GitHub
workflow — `ci.yml` runs lint/build/test/docker only, and this change deliberately does not touch
it. So the anti-masquerade gate is *real but manual*: it fails on an `automated_candidate` block
whenever it is run, and the updater workflow surfaces the state in its job summary, but no
required status check enforces it on a pull request today. The automatic safeguards are the Draft
status, the banner, and the block sitting in the diff. Wiring the validator into `ci.yml` as a
required check is the natural follow-up and needs both a `ci.yml` edit and a repository-settings
change — neither of which an agent may perform under this project's governance.

## Links

- `docs/agents/skills-update-automation.md` — operational detail
- `docs/agents/skills-update-providers.json` — the provider registry
- `.github/workflows/skills-update.yml`, `scripts/skill_updates/`
- ADR-0001 (vendoring rationale), ADR-0002 (authority vs workflow separation)
