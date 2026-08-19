# Automated skill update candidates

How this project detects that a vendored skill provider has moved upstream, and what it is — and
is not — allowed to do about it.

- **Workflow:** `.github/workflows/skills-update.yml`
- **Code:** `scripts/skill_updates/` (pure Python core) plus two entry points,
  `scripts/check-skill-updates.py` and `scripts/prepare-skill-update.py`
- **Registry:** `docs/agents/skills-update-providers.json`
- **Plugin inventory:** `docs/agents/skills-update-plugins.json` (ships **empty**)
- **Decision record:** `docs/adr/0003-automated-skill-update-candidates.md`

## Three surfaces, kept apart

"Skills" means three different things in the Claude Code ecosystem, with different distribution
mechanisms and different update stories. Conflating them is how a monitoring design ends up
claiming coverage it does not have, so this project separates them explicitly:

| | Surface | Where it lives | What this project does |
| --- | --- | --- | --- |
| **A** | Repo-vendored **project skills** | `.claude/skills/**`, in this git tree | **Owned and updated here.** Everything below about candidates, blocked conditions and the state machine is surface A. |
| **B** | Native **marketplace plugins** | A Claude Code user cache, outside this repository | **Monitored, never mutated.** Schema and comparison logic ship ready; the inventory is empty and no plugin is installed. |
| **C** | **Claude.ai custom ZIP skills** | Uploaded through the claude.ai web interface | **Documentation and package output only.** There is no documented programmatic upload, so nothing about C is automated. |

The bot acts on A. It reports on B. It does not touch C.

## The problem this solves, and the one it does not

This project vendors 81 skills from six upstream providers into `.claude/skills/**`. Vendoring
converts "trust upstream's `main` on every future run" into "trust upstream as of a specific
reviewed SHA". That is the right trade, but it has a standing cost: **someone has to notice when
upstream moves**, and that someone was previously a human remembering to look.

The bot removes the *noticing* and the *mechanical preparation*. It does not remove — and cannot
remove — the review. Every candidate it produces is explicitly unaudited, and the repository's own
governance validator fails while that state persists. See "Why a candidate cannot pass as
audited" below; that is the load-bearing property of the whole design.

## What runs, and when

| Trigger | Default mode | Effect |
| --- | --- | --- |
| `schedule`, daily at 05:37 UTC | `prepare` | Check all providers; open a Draft PR or an issue per drifted provider |
| `workflow_dispatch` | `check` | Whatever you pick: `provider` = `all` or one key, `mode` = `check` or `prepare` |

The non-round minute is deliberate: `:00` is the most contended slot on GitHub's scheduler and
the one where cron runs are most often delayed or dropped.

**When nothing has drifted** — the normal daily outcome — the run exits successfully having
created no branch, no PR, no issue and no comment. It writes a short job summary listing each
provider and its pinned commit, and stops. The check phase costs one `git ls-remote` per
provider; a provider that has not moved is never cloned.

## The candidate state machine

Every provider check ends in exactly one state (`scripts/skill_updates/states.py`):

| State | Meaning |
| --- | --- |
| `NO_DRIFT` | the reviewed ref resolves to the pinned commit; nothing to do |
| `DRIFT_DETECTED` | the ref moved; classification has not finished (never a legitimate end state) |
| `PREPARED_AUDIT_REQUIRED` | a mechanically clean candidate exists and needs a human audit |
| `BLOCKED` | a judgement call is required; no candidate was produced |
| `AUDITED` | a human, or an agent under a task contract, established trust |

**Automation can create exactly one of these: `PREPARED_AUDIT_REQUIRED`.** `AUDITED` is
unreachable from this package — `states.automation_may_set()` returns False for it,
`assert_automation_may_set()` raises, only `PREPARED_AUDIT_REQUIRED` has a declared transition
into it, and the governance validator fails on any manifest carrying a candidate block at all.
`AUDITED` is reached by *deleting* the block after a review, never by writing the word: a manifest
that writes `"state": "AUDITED"` is caught and named as taking the one route that cannot grant a
review.

`BLOCKED` beats `PREPARED_AUDIT_REQUIRED` when both apply, so a provider that produced some file
verdicts before hitting a blocking condition can never be reported as a usable candidate.

## How upstream moved: ancestry

"Upstream moved" is not one situation. Before anything is merged, the relation between the pinned
commit and the target is classified (`scripts/skill_updates/ancestry.py`):

| Relation | Meaning | Outcome |
| --- | --- | --- |
| `equal` | the ref still resolves to the pinned commit | `NO_DRIFT` |
| `fast-forward` | the pinned commit is an ancestor of the target | the only case a candidate is prepared |
| `diverged` | both sides hold commits the other lacks (includes a ref reset *backwards*) | **BLOCKED** |
| `rewritten` | no common ancestor at all — the history was replaced | **BLOCKED** |
| `unreachable` | the pinned commit is gone from upstream (force-push, deleted branch) | **BLOCKED** |

A force-push that replaces reviewed history with different content of the same shape is exactly
the supply-chain event a pinned vendoring model exists to catch, and it is **invisible to any
check that only compares tree contents** — every hash would still validate against the new
manifest. Only the ancestry relation catches it.

Two related disciplines: the ref is always resolved to a **full 40-hex SHA** and compared at full
length (a short-SHA comparison can collide, and the collision reads as "no drift" — silence
instead of a report), and the remote's **default branch** is read from `ls-remote --symref` rather
than assumed. Nothing hardcodes `main`: each provider's reviewed ref comes from the registry, and
a reviewed ref that has quietly stopped being the default branch is **BLOCKED** so the project
cannot drift onto an abandoned line of development without noticing.

## Outcomes

Each provider is processed independently, so one blocked provider never prevents another's
candidate.

**Clean update → one Draft PR per provider.** Branch name is
`automated/skills-update/<provider>-<sha12>`, derived only from an allowlisted provider key and a
validated 40-hex SHA. The PR body opens with:

> AUTOMATED UPDATE CANDIDATE — AUDIT REQUIRED — DO NOT MERGE YET.

**Blocked update → one deduplicated issue.** Title `Skills update blocked: <provider> -> <sha8>`,
carrying the exact old and new SHAs, the paths involved, and the reason. Nothing is written to the
tree, and no partial or conflicted PR is ever opened. If the condition persists, the daily run
finds the existing issue and — because the body is deterministic and contains no timestamp —
leaves it untouched rather than re-notifying everyone watching the repository.

**Stale issues are superseded, not accumulated.** The target SHA in the title is what makes
deduplication correct for a single upstream head — and is also what would otherwise file a *new*
issue every time upstream moves under a persistent block. Since a reworded `description` blocks
(it is trigger surface), an actively maintained provider would produce one a day. So when a newer
head blocks, older open issues for the same provider are closed with a pointer to the current one.
Each issue's evidence stays pinned to the commit it was written about, and only the newest stays
open. Blocked and discovery issues supersede independently — they mean opposite things about
whether the provider can still be updated, so one must never close the other.

**An unresolvable ref is a blocked outcome, not silence.** A provider whose reviewed ref cannot be
resolved has no target commit, so it is blocked with `drifted` false. It still reaches the
publication job and still opens its issue; gating publication on drift alone would have made an
unreachable or renamed upstream produce a green run and no report.

**One provider's failure never costs the others their report.** Analysis is isolated per provider:
an exception becomes that provider's own `unprovable` block rather than aborting the run, which
would otherwise leave every other provider with no text report, no job summary and no JSON.

**Deduplication is a read, not a bookkeeping file.** Before creating anything the bot asks GitHub
whether the branch, PR or issue already exists. State lives in GitHub, so re-runs are naturally
idempotent, nothing goes stale, and **nothing is ever force-pushed**. A closed-unmerged PR still
suppresses re-creation: that candidate was refused by a human, and re-opening it nightly would be
the bot arguing with a decision it has no standing to revisit.

## How a refresh is computed

For every vendored upstream-origin file, three byte strings:

```
BASE    bytes at the OLD pinned upstream commit (addressed by the manifest's upstream_blob_sha)
OURS    the bytes currently vendored here (may carry local patches)
THEIRS  bytes at the NEW upstream commit
```

resolved by four rules in order:

| Condition | Result |
| --- | --- |
| `THEIRS == BASE` | retain OURS — upstream did not touch the file |
| `OURS == BASE` | take THEIRS — never patched locally, adopt upstream verbatim |
| `OURS == THEIRS` | either — both sides converged |
| otherwise | deterministic three-way merge; clean → merged bytes, else **BLOCKED** |

The first three are byte comparisons evaluated before any decoding, so a binary or undecodable
file still resolves correctly whenever only one side moved. The merge engine is reached only when
*both* sides changed, which is the only case where a binary file can block.

BASE is taken from the manifest's recorded `upstream_blob_sha` — a content address — and
cross-checked against what the pinned commit actually holds at that path. If the two disagree the
manifest does not describe the commit it claims to pin, which is an integrity failure rather than
an update to merge. OURS is likewise verified against its recorded `vendored_blob_sha` before
anything is merged, because the bot may run against a working tree someone has edited.

### The merge engine

`scripts/skill_updates/merge3.py` implements diff3 over `difflib.SequenceMatcher` rather than
shelling out to `git merge-file`. The reason is reproducibility: `git merge-file`'s output is a
function of the installed git version's xdiff implementation, and a candidate that merges cleanly
in CI but conflicts on a maintainer's laptop would make the bot's verdicts unreproducible.
`difflib` is pinned to the Python version and `autojunk` is disabled, so alignment is a pure
function of content.

Two byte-level policies:

- **No normalization, ever.** No newline conversion, no transcoding, no trailing-newline
  insertion. Lines are split on `\n` only — deliberately *not* `str.splitlines()`, which also
  breaks on `\x0b`, `\x0c`, `\x1c`–`\x1e`, U+0085, U+2028 and U+2029. A "helpful" normalization
  would change `vendored_blob_sha`, would be indistinguishable from tampering, and would quietly
  alter licence files whose byte-identity these providers' ledgers assert as a redistribution
  claim.
- **Conflict markers are never written into a vendored file.** A conflict is reported as a
  blocked condition for a human to resolve, not as a mess handed to the next reader.

### Provenance the bot regenerates

`upstream_commit`, `upstream_tree`, per-file `upstream_blob_sha` / `upstream_mode` /
`vendored_blob_sha` / `vendored_mode`, and per-file and per-skill `locally_modified` / `patch_ids`
— all recomputed from the bytes the run actually produced, never carried forward.

`upstream_version` (where a provider records one) is **re-read from upstream** at the target
commit, from `.claude-plugin/plugin.json` then `package.json`, mirroring the same precedence used
for plugins. If it cannot be resolved there, the key is **deleted** and a note says so. Carrying
it forward is the nastiest kind of staleness: because the line does not change, it never appears
in the candidate diff at all, so a reviewer would see a manifest asserting a version belonging to
the superseded commit with nothing to notice. (This is not hypothetical — the first live run after
the fix found compound-engineering had moved `3.22.0` → `3.22.1`.)

Judgement fields are never regenerated: `reviewed_at`, `reviewed_by`, `classification`,
exclusion verdicts and prose notes carry through untouched, because no automated run has the
standing to change them. The one exception is `scripts_audited`, which is *withdrawn* rather than
carried when the candidate rewrites the script it attests to.

## Blocked conditions

An update is refused — no branch, no PR, no partial write — when any of these fire. Each exists
because the right answer depends on *reading* something, and a machine picking a side would be
manufacturing a review that never happened.

| Code | Condition |
| --- | --- |
| `unprovable` | The upstream ref or commit could not be proven |
| `ancestry` | History is not a fast-forward (diverged / rewritten / unreachable), or the reviewed ref is no longer the default branch |
| `skill-set` | A selected skill was added, deleted or renamed upstream |
| `inventory` | A selected skill's file inventory changed (**including a new file inside an installed skill**), or a vendored file drifted from its recorded hash |
| `licence` | Licence text or presence changed |
| `executable` | A symlink, submodule, or **newly** executable file appeared |
| `authority` | A skill's frontmatter trigger/authority surface changed |
| `conflict` | Three-way merge conflict, or a binary file changed on both sides |
| `patch-map` | A local patch id no longer maps onto the merged file |
| `closure` | Merged content references a file that is not vendored |

### New upstream content, three cases

The three are deliberately different, because treating them alike gets one of them wrong:

1. **A new file inside an installed skill or its dependency closure → BLOCKED.** Whether it
   should be vendored is a judgement call about that skill's closure.
2. **An installed skill removed or renamed → BLOCKED.** The reviewed thing no longer exists.
3. **A new *sibling* skill outside the installed selection → `DISCOVERY_REQUIRED`, not blocked.**
   A deduplicated issue titled `Skills discovery required: <provider> -> <sha8>` records it, and
   **the provider's other updates continue**. Refusing an otherwise-clean refresh because upstream
   added an unrelated skill would make every actively maintained provider permanently
   un-updatable. New skills are never installed automatically: adopting one means reading it,
   judging its authority surface, and writing a manifest entry — none of which is mechanical.

## EVAL_REQUIRED: what provenance cannot prove

Static gates answer *which bytes changed, and were they reviewed at the old commit*. They cannot
answer *does this skill still behave the same way*. A reworded instruction, a re-ordered agent
topology, or a changed script can alter behaviour while every hash checks out.

So a candidate whose changed files include a `SKILL.md`, anything under `agents/`, `hooks/`,
`scripts/`, `commands/`, `prompts/` or `rules/`, or any script-ish file, is marked
**`EVAL_REQUIRED`**. The marker lands in the candidate manifest's `automated_candidate` block and
in the PR body, together with generated old-vs-candidate instructions: check out the reviewed
state, exercise the affected skills, then do the same on the candidate **in a fresh session**, and
compare trigger, workflow, output and authority.

**Evals are never run in GitHub Actions.** They cost model time and money and need a clean session
with the candidate skills actually loaded; a scheduled CI job has none of those properties.
Writing the instructions down is the honest alternative to letting a green provenance run be
mistaken for a behavioural guarantee.

## Native plugin monitoring (surface B)

No plugin is installed by this work and none is installed by the bot.
`docs/agents/skills-update-plugins.json` ships with `"plugins": []`, so every plugin check is a
no-op that reads one small JSON file.

Version resolution mirrors Claude Code's documented precedence exactly:

    plugin.json version  >  marketplace.json version  >  git source commit SHA  >  unknown

The disagreements are the point. An **unchanged version over a changed source commit** is the
dangerous direction — the label stands still while the bytes move, and nothing about the label
proves the content was reviewed. A **bumped version over unchanged bytes** is harmless in itself
but means the version label is not a reliable change signal for that plugin. Neither is resolvable
by comparing version strings, which is why the source commit is compared independently.

Any change to the component surface is audit-required: `skills`, `agents`, `hooks`,
`mcp_servers`, `lsp_servers`, `monitors`, `bin`, `settings`, `dependencies`, plus projected
context cost. Each either adds behaviour, adds authority, or spends context on every session.

**What the bot never does here:** run `claude plugin update` (or any `claude` subcommand), install
Claude Code in CI, mutate a real plugin cache, or execute plugin code. An optional adapter reads
*captured* `claude plugin list --json` / `claude plugin details` output committed as fixtures, so
plugin monitoring is exercisable in CI with no credentials and no Claude Code present.

### Native auto-update behaviour, accurately

- Anthropic's own marketplaces **auto-update by default**.
- Third-party marketplaces default to auto-update **off**.
- An updated plugin is **not live in a running session** until `/reload-plugins` or a new session;
  the session keeps whatever it loaded at startup.

For any project-governed audited plugin, native silent auto-update should be **disabled** so
changes arrive through the monitored candidate flow and get a human audit — the same posture that
makes vendoring worthwhile for surface A.

## After a merge: verifying in a fresh session

Skills are loaded when a session starts. After a project-skill update merges, the session that
reviewed it is still holding the old bytes, so verification means **starting a new session** and
re-invoking the affected skills — confirming they still trigger when expected, still decline what
they used to decline, and still produce the same shape of output. When plugin components change,
run `/reload-plugins` or start a new session for the same reason.

Reasons are reported in that fixed order, so an unchanged situation renders an unchanged issue
body and deduplication works by comparison rather than bookkeeping.

**Licence coverage spans all three layouts these providers use**, which is worth stating because a
licence check that quietly applies to only one of them is worse than none — the PR body tells the
reviewer the licence *was* checked. The three: mattpocock's single shared notice recorded in the
manifest's `license` block (it lives outside every skill directory, so it appears in no `files[]`
array); anthropic's per-skill `LICENSE.txt` vendored from the skill's own subtree with
`origin: upstream`; and the root `LICENSE` that trailofbits, compound-engineering, awesome-copilot
and builderio copy into each skill directory with `origin: local` — 55 skills whose notices are
reached only because local-origin entries still record an `upstream_path`.

Three more deserve their rationale spelled out.

**`executable` fires on *newly* executable files only.** Several providers legitimately ship
`100755` scripts that this project vendors `100644` under a documented `*-mode-normalize` patch
id. Blocking on the mere presence of `100755` would refuse every compound-engineering update
forever while telling the reader nothing new. What is blocked is a file becoming executable
*between the pinned and target commits* — a capability appearing where a reviewer last saw none.
Symlinks and submodules are refused unconditionally.

**`authority` is the full trigger surface**, and it deliberately **includes `description` and
`when_to_use`**: `name`, `description`, `when_to_use`, `disable-model-invocation`,
`user-invocable`, `allowed-tools`, `disallowed-tools`, `model`, `effort`, `context`, `agent`,
`hooks`, `paths`, `shell`, `argument-hint`, `type`, plus the frontmatter key set itself (a new key
is a new surface even when this tool does not yet know what it means).

`description` and `when_to_use` are what the model reads to decide whether to invoke a skill at
all, so an upstream rewording changes *when the skill fires* even when it reads as ordinary prose.
A mechanical checker cannot tell a clarifying reword from one that widens the trigger. The cost is
real and accepted: more updates need a human, and an actively maintained provider will block often.

It is checked in *both* directions: BASE→THEIRS catches what upstream changed, and OURS→merged
catches what we might lose. The second direction is not redundant — this project's most important
local patches are frontmatter lines (`disable-model-invocation: true` on `wizard`,
`resolving-merge-conflicts`, `writing-for-agents`, `skill-creator-anthropic` and others), and a
frontmatter line cannot carry an HTML-comment patch marker, so the patch-id survival check is
structurally blind to exactly those patches.

## Why a candidate cannot pass as audited

This is the property everything else is arranged around.

1. The bot writes an `automated_candidate` block into the candidate manifest, recording
   `state: PREPARED_AUDIT_REQUIRED`, the superseded commit, and the target commit.
2. `scripts/validate-agent-governance.py` **fails** (`no-unaudited-update-candidate`) while that
   block is present — regardless of its `state` value, because the *key* is what fails, not the
   string.
3. `reviewed_at` and `reviewed_by` are left exactly as they were. They are true statements about
   the previous pin, and rewriting them to today's date would be the bot asserting a review it did
   not perform. The block records that those fields refer to the superseded commit.
4. If the candidate changes the bytes of a script in a skill carrying `scripts_audited: true`,
   that attestation is **withdrawn** (`scripts_audited: false`, plus
   `scripts_reaudit_required`). The validator's usual defence — a `vendored_blob_sha` mismatch
   meaning "re-audit, not just re-hash" — cannot fire when the bot is itself the rehash, and this
   survives even after someone deletes the `automated_candidate` block.

Clearing the candidate state therefore requires a deliberate human act: read the diff, re-assert
any withdrawn script audit, record a fresh `reviewed_at`/`reviewed_by`, and delete the block.

Note that a PR opened by `GITHUB_TOKEN` does not trigger further workflow runs (GitHub's recursion
guard), so an initial candidate PR does not necessarily start CI by itself; that recursion-guard
behaviour is GitHub's own and may evolve, and nothing here depends on which way it goes. What is
enforced independently of it: `.github/workflows/ci.yml` carries an independent `governance` job
that runs `scripts/validate-agent-governance.py` (plus its self-tests and the updater regression
suite) on ordinary `pull_request`/`push` CI, so whenever normal CI does execute on an unaudited
candidate's head, that job is expected to fail on the `automated_candidate` block. The candidate
stays a Draft that a human must audit; only the audited commit — the block deleted, any withdrawn
script attestation re-asserted, `reviewed_at`/`reviewed_by` refreshed — is expected to pass the
`governance` job. Whether a given candidate's first CI run happens automatically, is suppressed by
the recursion guard, or needs an approval click is a GitHub-behavior detail that does not change
this authority/state-machine boundary either way.

Separately, `.github/workflows/skills-update.yml`'s own read-only `check` job runs an integrity
preflight — `--self-test-hook` (which still exercises the live `automated_candidate` check, not
just the hook's own self-test), the mutation-probe anchors, and the updater regression suite —
before it reports drift for any provider. This is a deliberately narrower set than `ci.yml`'s
`governance` job: it does not also run `--self-test` (the offline fixture matrix that regression-
tests the validator's own check logic), because ordinary PR/push CI already covers that matrix on
every change to `main`, and the daily/dispatched updater run does not need to duplicate it. That
is a different enforcement point from the `governance` job, not a redundant copy of it: it stops
this workflow's elevated `publish` job (the one with `contents: write` / `pull-requests: write` /
`issues: write`) from starting when the updater's own tooling is red, independent of whether any
particular candidate PR happens to get a governance run. Neither preflight ever runs the full
mutation-probe rewrite (`scripts/skill_updates/tests/mutation_probe.py` with no flag) — that stays
a separate, deliberately local/deep verification step, never ordinary CI.

## Security posture

**Upstream is data, never executable input.** Provider repositories are fetched into a *bare*
repository and read through `git cat-file`. No working tree is ever created, so upstream content
never lands on disk with a name, a mode or an execute bit — a hostile path such as
`.git/hooks/pre-commit` is just a string in a listing. No upstream script is ever run, including
to assess it.

Every git invocation is an argument list (never a shell string) with a hardened environment:
ambient `~/.gitconfig` and `/etc/gitconfig` are neutralized, hooks are pointed at a nonexistent
directory, symlink creation is off, credential prompting is disabled, LFS smudging is skipped, and
the transport allowlist is `https` alone for upstream. The `ext::` transport — the only one that
turns a URL into a command — is refused unconditionally, everywhere.

Inputs are allowlisted rather than sanitized, so a value outside the allowlist aborts the run
instead of being escaped: upstream URLs must match `https://github.com/<owner>/<repo>` exactly,
commits must be full 40-hex, refs must be plain branch names (and may **not** be a SHA — that
would freeze drift detection permanently), and provider keys are `[A-Za-z0-9-]` starting with an
alphanumeric. Branch names and issue titles are built only from those validated parts.

**Workflow hardening.** Only `schedule` and `workflow_dispatch` — never `pull_request_target`, so
fork code can never run in a privileged job. Top-level `permissions: {}`; the check job holds
`contents: read` and nothing else; only the publication job holds `contents: write`,
`pull-requests: write`, `issues: write`. `GITHUB_TOKEN` only — no PAT, no GitHub App, no new
repository secret. `actions/checkout` is pinned to a full commit SHA and is the *only* action
used; Python comes from the runner image, so there is no `pip install`, no `npm`/`npx`, and
nothing piped into a shell. Dispatch inputs are enumerated `choice` values and reach scripts
through `env:`, never interpolated into `run:` text — that inline substitution is the Actions
script-injection sink. A `concurrency` group prevents overlapping runs and never cancels one in
flight. Temporary clones live under `tempfile.TemporaryDirectory()` and are removed on every exit
path, including exceptions.

### Repository setting required for pull requests

GitHub blocks Actions from opening pull requests unless this is enabled:

> **Settings → Actions → General → Workflow permissions**
> ☑ *Allow GitHub Actions to create and approve pull requests*

When it is off, PR creation returns 403 and the workflow **fails loudly** with that exact
remediation text. The candidate branch has already been pushed at that point, so nothing is lost:
a maintainer can open the PR by hand, or enable the setting and re-dispatch. **The bot never
changes repository settings and never routes around the restriction with a PAT or an App token.**

## The audit-only monitor

`BuilderIO/builder-agent-skills` is in the registry with `monitor_only: true`. Nothing from it is
vendored and the bot will never prepare a candidate for it — the `prepare` path for a monitor
provider can only report or open an issue; there is no code route from it to a file write.

It was reviewed and rejected on provenance grounds: at the baseline commit there is no root
licence, and the tree's only licence file is an MIT `hallmark/LICENSE` held by "Hallmark
contributors", which by directory convention plausibly covers `hallmark/` alone (see
`docs/agents/builderio-skills-policy.md`). The registry records `watch_baseline` with **explicit
nulls** for the absent root-licence paths, because that absence *is* the reviewed finding: a root
licence appearing is the signal worth waking a human for, and the condition under which the
exclusion decision would be worth revisiting — by a human, in a separate review. Ordinary commits
to that repository produce no output at all.

## Running it by hand

```bash
# Report drift for every provider. Read-only: no branch, PR, issue or comment.
python3 scripts/check-skill-updates.py --all
python3 scripts/check-skill-updates.py --provider trailofbits --json

# Classify and report one provider without writing anything.
python3 scripts/prepare-skill-update.py --provider mattpocock --dry-run

# Write the candidate into the working tree (still no GitHub calls).
python3 scripts/prepare-skill-update.py --provider mattpocock
```

Exit codes: `0` the tool did its job — including "no drift" and "blocked, issue recorded", because
a blocked provider is the bot working correctly and a permanently red scheduled workflow trains
people to ignore it. `1` the tool could not do its job (bad config, unreadable manifest, git or API
failure). `2` command-line misuse. `--fail-on-blocked` inverts the first case for ad-hoc runs.

## Tests

```bash
python3 -m unittest discover -t scripts -s scripts/skill_updates/tests
python3 scripts/validate-agent-governance.py
python3 scripts/validate-agent-governance.py --self-test
python3 scripts/validate-agent-governance.py --self-test-hook
python3 scripts/skill_updates/tests/mutation_probe.py --check-anchors
```

Most of those now also run as ordinary CI, in two places, though not identically in both.
`ci.yml`'s independent `governance` job runs the updater suite, `--self-test-hook`, `--self-test`,
and the anchor check — four separate steps, on every `pull_request`/`push`. `--self-test-hook`
alone already covers the bare `validate-agent-governance.py` invocation above (its live check
falls through to the same `ALL_CHECKS` loop plus the hook's own self-test), so `ci.yml` does not
run the bare command as a fifth, separate step. `skills-update.yml`'s read-only `check` job runs a
deliberately narrower integrity preflight — `--self-test-hook`, the anchor check, and the updater
suite, but not `--self-test` — before it reports drift for any provider, gating whether the
elevated `publish` job can start; see "Why a candidate cannot pass as audited" above for why that
narrower set is still sufficient for its purpose. Running the full command list by hand, as above,
stays the fastest way to reproduce a CI failure locally and is what this document continues to
recommend during development. The full mutation-probe rewrite (no flag) is
excluded from both CI paths on purpose — it mutates tracked files in place — and stays a separate
local/deep verification step; see "Running it by hand" above for the read-only commands.

Every test is offline and hermetic: upstream repositories are real local git repositories built in
a temporary directory, and GitHub is a fake adapter that mirrors the real deduplication rules. The
suite covers no drift, provenance-only drift, an unmodified file changed upstream, a clean
three-way merge preserving a local patch, merge conflict, binary conflict, add/delete/rename,
licence change and disappearance, newly executable files and symlinks, frontmatter authority drift
in both directions, patch-id survival, duplicate branch/PR suppression, no-op idempotence,
injection-shaped inputs, and the candidate-cannot-masquerade-as-audited guarantee. Merge
properties are checked over a seeded random corpus: clean output is always valid UTF-8 and free of
conflict markers, one-sided changes always yield that side, conflict detection is symmetric under
swapping OURS and THEIRS, repeated runs are byte-identical, and **no line in a merged result is
ever fabricated** — every one came from BASE, OURS or THEIRS.

## Prior art: researched, not depended on

Several third-party skill-updater projects were reviewed as **design evidence only**:
`yizhiyanhua-ai/skills-updater`, `803/skills-supply`, Auto-Claude Updater, Skill.Fish,
`skills.sh`, `skillsmp`, and `npx skills`.

**None of them is vendored, installed, executed, or depended on**, and no code was copied from
any of them. Two of them — `yizhiyanhua-ai/skills-updater` and `803/skills-supply` — did not
expose a complete recognized root licence at the reviewed state, which independently rules out
copying from them; the rest were simply not needed. This tool has no third-party dependency at
all: Python standard library and `git`.

What was worth borrowing was a set of **concepts**, all of them implemented from scratch here:

| Concept | Where it lands |
| --- | --- |
| Declarative desired state | the provider registry + manifests describe the intended pin |
| Managed-file ownership | `files[]` says exactly which paths the bot may touch |
| Dry run | `--dry-run` on `prepare`, and `check` is read-only by construction |
| Idempotent reconciliation | re-running over an unchanged tree writes nothing and produces a byte-identical manifest |
| Human **and** JSON reports | `text_report()` / `to_json()` from the same analysis |
| Protect unknown/manual files | anything not in `files[]` is never written; an undeclared on-disk file fails the governance gate |
| No-op silence | no drift → no branch, no PR, no issue, no comment |
| Version compatibility matrix | the plugin version-precedence rules |

**The canonical state stays the repository's own artifacts** — the provider manifests, the patch
ledgers, and the actual git tree. There is deliberately **no hidden state file** that could
override them or drift from them: deduplication is a read against GitHub, and "what we trust" is
the manifest, in the diff, where a reviewer sees it.

## Design boundaries

The Python core is a pure function of (repo bytes, upstream bytes, config); `ghadapter.py` is the
only component that talks to GitHub, and `publish.py` the only one that writes to git. That split
is what makes the whole classifier testable without a network. `publish.py` contains no `--force`,
`--force-with-lease`, `+refspec`, `--delete` or `--mirror` in any git invocation, and a test
asserts that by parsing the module's AST rather than grepping it.

Adding a provider means adding a registry entry and a manifest — not editing the classifier. The
registry is cross-checked against the manifests at load time, so a config that drifts from them
fails closed rather than producing a confidently wrong comparison later.
