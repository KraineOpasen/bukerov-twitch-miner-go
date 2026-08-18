# Anthropic skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of [anthropics/skills](https://github.com/anthropics/skills)
into `.claude/skills/` instead of installing it as a live Claude Code plugin. This document is the policy for
what's installed, why, how it's patched, and how to update it. See also
`docs/agents/anthropic-skills-manifest.json` (machine-readable inventory, file-level) and
`docs/agents/anthropic-skills-patches.md` (per-patch ledger). This is a separate vendored set from
`docs/agents/mattpocock-skills-policy.md` — the two upstreams, manifests, and ledgers are independent and
never share a skill name (see `scripts/validate-agent-governance.py`'s `manifest-ownership-partition` check).

## Upstream

- Repo: `https://github.com/anthropics/skills`
- Reviewed commit: `89dcaa3a283f79ed84fd8fe53e2208b9442a6427` (advanced from
  `f6656c1256d5a8adfa37db9110046ef20bac644c` by a **provenance-only** audit of the machine-prepared candidate
  on PR #176: `f6656c12 -> 89dcaa3a` is a true fast-forward — one commit, merge-base equal to the old pin, no
  reverse commits, no merges — and its complete upstream delta is three paths, `.claude-plugin/marketplace.json`
  and the two files of a new sibling skill, `skills/claude-academy-guide/{LICENSE.txt,SKILL.md}`. None of them
  lies under an installed subtree, so zero vendored bytes changed. The three installed skill trees were compared
  in both directions at both pins — identical subtree SHAs and identical complete recursive inventories, every
  path, mode and blob SHA equal, no symlink, submodule or executable-bit change, both Apache-2.0 `LICENSE.txt`
  notices unchanged. The installed prose and scripts were therefore **carried forward on demonstrated byte
  identity, not re-read end to end**; the standing end-to-end read is the one each skill's `audit_ref` records)
- The pin before that, `f6656c12`, was itself refreshed from `b29e7cf65e5cb78a5ac33d582270551bc74a14eb`, also as a
  provenance-only bump: the full selected subtree — all three vendored skills' complete directory trees, including
  every `.py`/`.html`/`.sh` file — was re-audited and found byte-identical between those two commits, with zero
  content or patch changes
- Reviewed tree SHAs (per vendored skill directory), unchanged from `f6656c12` and re-proven against
  `89dcaa3a`: `skill-creator` `3cf9a8db32597ba3e24b584a3d696f4e11c7d7b6`, `frontend-design`
  `0d5b74a14bdf3ebcd64f352d06376a2ef05ed296`, `webapp-testing` `5ffb7dc66b9fd4c25c3e400a4c00da99a349b714`.
  This map is keyed on **upstream** directory identity, not on the vendored directory name — which is why
  `skill-creator` appears here rather than `skill-creator-anthropic` (see "Installed: 3 skills" for the rename)
- Current upstream HEAD at review time: `f379e5ad66e2febc1616cf8d6284666fecbe514e` (**drift: ahead of pin, not
  audited**, observed 2026-08-18T05:44:38Z). It is a fast-forward extension of the reviewed pin, not a rewrite —
  `89dcaa3a` is still a true ancestor — and its complete delta is again three paths: `.claude-plugin/marketplace.json`
  metadata plus a new sibling skill, `skills/discernment-nudge/{LICENSE.txt,SKILL.md}`. It touches none of the
  three installed subtrees. `discernment-nudge` was **not** reviewed, adopted, held or excluded by this audit:
  adopting or declining a new skill is a judgement call, not a mechanical one, and it gets its own
  `DISCOVERY_REQUIRED` issue and its own candidate under "Update procedure" below
- Upstream's own `README.md` notes that `skills/docx`, `skills/pdf`, `skills/pptx`, and `skills/xlsx` are
  **source-available, not open source** — reference copies of the skills powering Claude's built-in document
  capabilities, shared for developers to read but not under an open-source license. None of the three skills
  installed here are in that set, but none of those four skills are installed either way (see "Excluded"
  below) — they weren't requested by this integration's contract regardless of license.

## Installation model

**Project-local vendored copy**, not a live plugin install. Each skill's files are copied verbatim into
`.claude/skills/<name>/` at review time, then minimally patched (see below) and every file's mode is
normalized to `100644` (no executable bits — see "Local patches" and the manifest's `vendored_mode`).
`automatic_updates: false` — nothing about this installation re-fetches or re-syncs from upstream on its own.
A human (or an explicitly-contracted agent task) must re-run the review process to pick up a new upstream
commit.

## Installed: 3 skills

- **`skill-creator-anthropic`** (renamed from upstream's `skill-creator`) — creates and iterates on Claude Code
  skills, including an eval/benchmark loop and a description-optimization loop. Renamed to avoid colliding with
  the Claude Code built-in skill of the same name, and moved to **explicit-invocation-only**
  (`disable-model-invocation: true`) — a plain "create a skill" request should route to the built-in, not this
  vendored copy; use `/skill-creator-anthropic` to invoke this one directly.
- **`frontend-design`** — aesthetic-direction guidance for the dashboard's existing Go `html/template` +
  Tailwind + HTMX + ApexCharts UI. Model-invoked, scoped to this repo's actual stack (see
  `docs/agents/anthropic-skills-patches.md`'s `fd-stack-pin` row).
- **`webapp-testing`** — Playwright-based toolkit for exercising a locally running web app. Model-invoked,
  scoped to `localhost`/`127.0.0.1`/`file://` targets only.

## Excluded

- **14 non-installed promoted skills, reviewed at the current pin** — `algorithmic-art`, `brand-guidelines`,
  `canvas-design`, `claude-api`, `doc-coauthoring`, `docx`, `internal-comms`, `mcp-builder`, `pdf`, `pptx`,
  `slack-gif-creator`, `theme-factory`, `web-artifacts-builder`, `xlsx` — none requested by this integration's
  contract; `docx`, `pdf`, `pptx`, and `xlsx` are additionally source-available-not-open-source per upstream's
  README (see "Upstream" above). Full list with reasons in `anthropic-skills-manifest.json`'s `excluded_skills[]`.
- **1 discovery-reviewed exclusion, now inside the current pin** — `claude-academy-guide`
  (`skills/claude-academy-guide`), reviewed from discovery snapshot `89dcaa3a283f79ed84fd8fe53e2208b9442a6427`
  (Issue #175) while that commit was still ahead of the pin. `89dcaa3a` is now the pinned `upstream_commit`
  above — it is the very commit that adds this skill — so the skill is part of the reviewed tree and the
  verdict below is a verdict on a skill the pin describes. The verdict itself is unchanged by the pin advance.
  Recorded `EXCLUDE`: an end-user Claude Academy learning-recommendation skill (broad
  model-invoked Claude/Claude Code/skills/plugins/MCP/prompting trigger, runtime `academy.claude.com` catalog
  fetch) with no material-addition case for this repository's engineering-only scope. This entry does not
  change the installed count above; see the manifest's `excluded_skills[]` for the full reason.
- **Totals at the reviewed pin** — `89dcaa3a` promotes 18 skill directories under `skills/`: the 3 installed
  above and the 15 excluded across the two bullets here (14 + `claude-academy-guide`). The predecessor pin
  `f6656c12` promoted 17; the one commit between them is what added the 18th.
- **Non-skill upstream paths** — `spec/`, `template/`, `.claude-plugin/`, `README.md`,
  `THIRD_PARTY_NOTICES.md`, `.gitignore` — repo scaffolding and marketplace metadata, not skill content. See
  `excluded_upstream_paths[]` in the manifest.

## Invocation modes

- **User-invoked** (`disable-model-invocation: true`): `skill-creator-anthropic` — moved from upstream's
  model-invoked default by the `skc-user-invoked` patch.
- **Model-invoked**: `frontend-design`, `webapp-testing` — unchanged from upstream.

## License & attribution

All three vendored skills are Apache License 2.0. Obligations and how this vendoring satisfies them:

- **§4(a) (provide a copy of the License):** each skill directory carries its own verbatim `LICENSE.txt`.
- **§4(b) (mark modified files):** every locally patched file carries a `bukerov-local-patch` marker (Markdown
  `<!-- -->` comments, or `#` comments in Python) pointing at a ledger row in
  `anthropic-skills-patches.md` that says what changed and why — that ledger, together with the markers, is
  this project's "prominent notice" that the file was changed from upstream.
- **§4(c) (retain copyright/attribution notices):** untouched; the vendored `LICENSE.txt` files are not edited.
- **§4(d) (reproduce NOTICE, if any):** not applicable — upstream ships no top-level `NOTICE` file for this
  repo (see `THIRD_PARTY_NOTICES.md`, which is excluded as non-skill content per above, not a per-file NOTICE).
- `skill-creator`'s and `webapp-testing`'s `LICENSE.txt` carry the Apache License Appendix with
  `Copyright 2026 Anthropic, PBC.`; `frontend-design`'s `LICENSE.txt` ends at "END OF TERMS AND CONDITIONS"
  with no Appendix — the same copyright attribution is recorded on its behalf in the manifest's
  `license.copyright_notice_source` map instead of being invented into that file.

This is a **separate license/attribution obligation from** `docs/agents/mattpocock-skills-policy.md`'s Matt
Pocock skills (MIT-licensed) — the MIT `LICENSE` at `.claude/skills/LICENSE` covers that other vendored set,
not this one. This repository (`bukerov-twitch-miner-go`) has no stated top-level license of its own in scope
for this task; where relevant elsewhere in this project's history, an Apache-2.0-licensed dependency is
one-way compatible into a GPLv3 project (Apache-2.0 code can be included in a GPLv3-licensed work; the
reverse is not true) — noted here only as a standing compatibility fact, not a claim about this repo's actual
license.

## Local patches summary

14 patch ids touch `skill-creator-anthropic` (across `SKILL.md` and 6 scripts/HTML files), 3 touch
`frontend-design` (all in `SKILL.md`), and 6 touch `webapp-testing` (across `SKILL.md`, `with_server.py`, two
examples, and the new local test file). One further id, `anthropic-mode-normalize`, spans both script-bearing
skills: it records the `100755` → `100644` mode normalization described under "Installation model" above. That
normalization was always applied and always documented in prose, but had no id, so two content-unmodified
files recorded a real change to the vendored artifact with an empty `patch_ids`. It now has one, and
`provider-vendored-modes` fails closed on any undocumented mode difference in either direction. Full ledger,
one row per patch id per file:
`docs/agents/anthropic-skills-patches.md`. No patch translates or stylistically rewrites upstream text — every
change narrows a capability (background execution, auto-open, CDN/network fetch, tracker-mutation-shaped
writes into a git repo, shell-metachar execution, invocation scope) to match this project's governance model,
the same principle `mattpocock-skills-policy.md` uses for its own patch set.

### Default: minimal patching

Under Governance v3 (`docs/adr/0002-governance-v3-skill-native-orchestration.md`), skills are preserved as
close to their authors' intent as practical. **Do not patch a skill merely because it uses subagents, several
writers, reviewers/critics, parallel analysis, iterative fixes, or its own handoff/orchestration pattern** —
including fan-out that upstream leaves unbounded. That is engineering workflow, and workflow belongs to the
skill (see `docs/agents/agent-orchestration.md`). Patch only for concrete project incompatibility, a broken
dependency, license/provenance necessity, or a genuine authority/integrity boundary.

The `skc-agent-cap` patch id, and the concurrency-cap clauses inside `skc-change-mode-gate` and
`skc-runloop-foreground-sandbox`, were written under Governance v2's orchestration rules and would not be
justified by the v3 test above. They are left in place, byte-identical, and are candidates for removal at the
next re-vendor — each needs its own reviewed PR through the update procedure below.

## Governance precedence

Vendored skills sit **below** this project's own policy **on authority**. The authority chain has exactly four
levels (see `CLAUDE.md`'s "Claude Code Governance (v3)" section and `docs/agents/agent-orchestration.md`),
narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched (both this set and the Matt Pocock set).
4. **Generic model behavior** — fallback only.

Unpatched upstream text is **not** a separate tier below the patches: a vendored skill's instructions are
whatever its vendored bytes say, patched and unpatched alike, and they all sit together at level 3. Where a
local patch and the upstream text around it disagree, the patch wins — that is what patching means, and it is
resolved inside level 3 rather than by a fifth level. A skill instruction never overrides a
`.claude/rules/*.md` constraint or a hook denial.

**On workflow the order is inverted**: an invoked audited skill owns its documented engineering methodology —
agents, lanes, reviewers, writers, repair loops — and the project does not override it. See
`docs/agents/agent-orchestration.md`.

## Supply-chain assumptions

Same vendoring rationale as `mattpocock-skills-policy.md`: a live plugin install trusts upstream's default
branch on every future run, not just the commit reviewed today. Vendoring converts that into "trust as of a
specific reviewed SHA, re-established only when someone deliberately re-reviews." This set's manifest tracks
hashes **per file**, not just per `SKILL.md` (`mattpocock-skills-manifest.json`'s schema is `SKILL.md`-only) —
`skill-creator-anthropic` and `webapp-testing` ship real executable scripts (Python; `with_server.py` spawns
subprocesses and manages process groups), so the script content itself, not just the prose instructions, is
part of what a re-review must re-audit. `scripts_audited: true` in the manifest, with an `audit_ref`, records
that every `.py`/`.html`/`.sh` file in those two skills' `files[]` was read end-to-end **during the review that
`audit_ref` names**, not just diffed. It is that read the attestation stands on. A later provenance-only pin
advance carries the attestation forward only by proving those bytes did not change; it does not re-establish it
and does not claim a fresh end-to-end read (see "Upstream" above).

Every `files[]` entry, in every skill, carries a `vendored_blob_sha` — the `git hash-object` of the file as
committed to this repo right now. For an unmodified file this equals `upstream_blob_sha` (both are checked);
for a locally patched file or the local-origin test file, it's the only hash pinning that file's content, since
`upstream_blob_sha`/`patch_ids`/`reason` only describe the *fact* of a modification, not its exact bytes. This
closes a gap the original design otherwise had: before `vendored_blob_sha` existed, a patched or local-origin
file had no content-level integrity check at all — only "was it marked as modified" was verified, not "does it
still say what the ledger says it says." Now `scripts/validate-agent-governance.py`'s
`provider-file-hashes` check (formerly `anthropic-file-hashes-verified-locally`, generalized when the
validator's provider registry was made generic — same logic, now applied to every file-level provider) fails
closed on ANY on-disk edit to ANY vendored file (patched,
audited, or local-origin) that isn't accompanied by a deliberate `vendored_blob_sha` bump in the manifest —
which is exactly the re-audit forcing function: you cannot silently edit `with_server.py` (or any other
already-reviewed script) and have the validator stay green.

## Automated drift detection

`automatic_updates` stays **false**: nothing here is ever updated without review. What is automated is
*noticing*, and the mechanical half of preparing a re-vendor.

A scheduled workflow (`.github/workflows/skills-update.yml`) resolves this provider's reviewed branch —
recorded in `docs/agents/skills-update-providers.json`, which owns the ref while this manifest owns the
pin — to a concrete commit each day. When nothing has moved it does nothing at all: no branch, no pull
request, no issue, no comment. When something has moved it either opens **one Draft PR** carrying
refreshed bytes and regenerated provenance, or — if any judgement call is required — refuses entirely
and opens **one deduplicated issue** explaining why. It never opens a partial or conflicted PR.

A candidate it produces is **not** a reviewed pin. The manifest it writes carries an
`automated_candidate` block, and `scripts/validate-agent-governance.py` fails while that block is
present, so the candidate cannot pass the governance gate on automation alone. `reviewed_at` and
`reviewed_by` are left untouched, because they remain true statements about the superseded commit.
Clearing the candidate state — reading the diff, re-asserting any withdrawn `scripts_audited`, recording
fresh review fields, deleting the block — is the human step the update procedure below describes, and
the bot cannot perform it.

Upstream is read as data: repositories are fetched bare and read through `git cat-file`, never checked
out, and no fetched script is ever executed, including to assess it.

Three further rules bound what a candidate can be. **Only a fast-forward** from the reviewed
commit is ever prepared: if upstream's history diverged, was rewritten, or no longer contains the
reviewed commit, that is BLOCKED — a force-push that swaps reviewed history for different content
of the same shape passes every tree-content check, so the history relation is the only thing that
catches it. **The trigger surface is audit-required**, and it includes `description` and
`when_to_use`: those are what the model reads to decide whether to invoke a skill, so an upstream
rewording changes when the skill fires. And **provenance is not behavioural equivalence** — a
candidate whose changed bytes could alter behaviour is marked `EVAL_REQUIRED`, with old-vs-candidate
instructions to run in a fresh Claude session; the bot never runs evals itself.

A new skill appearing upstream *outside* this project's installed selection is not installed and
does not block this provider's other updates; it opens its own deduplicated `DISCOVERY_REQUIRED`
issue so adopting it stays a human decision taken on its own schedule.

Full detail, including the nine blocked conditions and the security posture:
`docs/agents/skills-update-automation.md`.

## Update procedure

1. Fetch the new upstream commit into a read-only clone (never edit it in place); set
   `GOVERNANCE_UPSTREAM_DIR_ANTHROPIC` to that clone's path for the validator's stricter blob-hash mode.
2. Diff the set of directories under `skills/` against the last-reviewed list — note additions, removals,
   renames among the three installed skills, and re-check whether any newly promoted skill should be added.
3. For each vendored skill, diff every file in its `files[]` list against the **currently-vendored copy** (not
   raw upstream — several files are locally patched) to isolate genuinely new upstream content from an old
   patch.
4. Re-run the same review judgment as the original vendoring for anything new: does it assume standing
   background execution, auto-open, external network fetch, or a write into a git working tree this project
   doesn't grant by default? If so, patch it the same way (minimal, marked, no rewrites, `scripts_audited:
   true` re-confirmed for any touched script) rather than installing it unpatched. The test is **authority**,
   not orchestration — a skill's agent topology, fan-out width, writer count, reviewer lanes and repair loops
   are not grounds for a patch (see "Default: minimal patching" above).
5. Update `upstream_commit`, `upstream_tree`, `upstream_current_head`, `drift`, and `reviewed_at` in
   `anthropic-skills-manifest.json`; update every touched file's `upstream_blob_sha`/`upstream_mode` and
   `locally_modified`/`patch_ids`. Recompute `vendored_blob_sha` (`git hash-object <path>`) for every touched
   file **last**, after all SKILL.md/script/doc edits for this round are finalized — this is what actually
   re-pins the file and clears the validator's fail-closed check.
6. Update `anthropic-skills-patches.md` for any patch that changed, was added, or was removed.
7. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
8. Open the change as its own dedicated Draft PR (see `mattpocock-skills-policy.md`'s "Dedicated Draft PR
   requirement" — the same rule applies here) — never bundle a skills re-vendor into an unrelated change.
9. Get human review before merge; this task's own governance forbids the agent from merging it itself.

## Rollback

1. Identify the last-known-good `upstream_commit` from `anthropic-skills-manifest.json`'s git history.
2. Restore the three affected `.claude/skills/<name>/` directories from that prior commit (`git checkout <sha>
   -- .claude/skills/skill-creator-anthropic .claude/skills/frontend-design .claude/skills/webapp-testing`).
3. Restore `docs/agents/anthropic-skills-manifest.json` and `anthropic-skills-patches.md` from the same commit.
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description.

## Trigger evals

Positive/negative expectations, useful when re-tuning any of these three descriptions:

| Skill | Should trigger | Should NOT trigger |
| --- | --- | --- |
| `skill-creator-anthropic` | Only the explicit `/skill-creator-anthropic` invocation. | A plain "create a skill for X" (routes to the built-in `skill-creator` instead — that's the point of the rename); "benchmark `internal/models`'s bet strategies" (that's `go test -bench`, not a skill-triggering eval). |
| `frontend-design` | "Restyle the dashboard's streamer detail page with a more distinctive look"; "the settings page looks templated, make it feel intentional." | Backend/API/schema work ("add a new settings field", "change the analytics query") — out of scope per `fd-stack-pin`; wanting several candidate directions at once — that's `prototype`; picking chart series colors — that's `dataviz`'s territory, not this skill's. |
| `webapp-testing` | "Click through the dashboard at localhost:8080 and confirm the notifications toggle works"; "screenshot the settings page after my CSS change." | "Add tests for the analytics repository" (that's `go test`/`tdd`, not browser automation); "debug why the dashboard shows stale data" (that's `diagnosing-bugs`'s loop — this skill only supplies browser evidence into it); anything naming a production or remote host — refused outright by the `webapp-testing-localhost-only` patch. |

The eval/benchmark loop inside `skill-creator-anthropic` never auto-starts for any of this — it only runs when
a user explicitly asks for it in the current session (per the `skc-change-mode-gate` patch).

## Known limitations

- **The `CLAUDECODE` nested-session guard is preserved, not bypassed** (`skc-py-no-claudecode-strip`): if
  `run_eval.py`/`improve_description.py`'s `claude -p` subprocess refuses to start because it detects it's
  already inside a Claude Code session, that failure surfaces as a visible error, not a silent workaround. This
  is by design — a hash mismatch or unexpected pass here is meant to be investigated, not patched around
  again.
- **xlsx inline preview is degraded** in the eval viewer after the SheetJS CDN removal
  (`skc-html-no-cdn`) — spreadsheet outputs show a "download the file instead" notice rather than a rendered
  table. Every other output type (text, image, PDF, binary) renders exactly as upstream.
- **PyYAML is required** by `quick_validate.py` and `package_skill.py` (both import `yaml`) — never
  auto-installed by any of this project's tooling; if it's missing, those two scripts fail loudly rather than
  the governance layer silently `pip install`-ing something.
- **The `claude` CLI and `lsof` are both expected on `PATH`, both for `skill-creator-anthropic`** — `claude`
  for the eval/description-optimization loop (`run_eval.py`/`run_loop.py`), and `lsof` for
  `eval-viewer/generate_review.py`'s `_kill_port()` helper (currently-unreachable behind the
  `skc-py-static-only-viewer` static-only gate, since it's part of the disabled server mode). `webapp-testing`
  uses neither; neither is auto-installed.
- **Playwright plus a pre-installed Chromium are expected** for `webapp-testing`
  (`PLAYWRIGHT_BROWSERS_PATH`) — the `webapp-testing-localhost-only` patch explicitly forbids running
  `playwright install`; if the browser is missing, the skill is expected to stop and report rather than fetch
  one.
- **`generate_review.py`'s HTTP server code remains in the file but is unreachable** behind the
  `skc-py-static-only-viewer` gate (`sys.exit(2)` unless `--static` is passed) — this was a deliberate choice
  to keep the patch minimal (disable the code path, don't delete dead code that upstream might still reference
  elsewhere) rather than a note that it's somehow still in play.
- **The `~/Downloads` pickup in the description-optimization flow is a user-driven browser-export step**, not
  an agent-initiated file write — `eval_review.html` (a static file this vendored copy already writes, not a
  server) exports `eval_set.json` via the browser's own download mechanism when the user clicks "Export Eval
  Set"; nothing here reads or writes `~/Downloads` on the agent's own initiative.
- **`with_server.py` rejects any compound/chained shell command in `--server` by design** (`&& || ; | & > <`
  backtick `$(`) — the old `cd x && ...` idiom must become `--cwd <dir>` instead; this is intentionally a hard
  refusal (exit 2), not a best-effort sanitization, because a shell-metacharacter denylist over free text can
  never be proven exhaustive (see `mattpocock-skills-policy.md`'s own hook-evasion discussion for the same
  reasoning applied to `.claude/hooks/governance-policy.py`).
- **This document's hook/permission-layer caveats are shared with `mattpocock-skills-policy.md`** — see that
  document's "Known limitations" section for the parts of this project's governance model (operation modes not
  mechanically enforced by the hook, MCP allowlist not pattern-matched, etc.) that apply equally to this
  vendored set and aren't repeated here.

## Compatibility

Reviewed and vendored against the same Claude Code version and governance-layer assumptions recorded in
`mattpocock-skills-policy.md`'s "Compatibility" section (frontmatter/permission/hook-payload behavior) — see
that document rather than duplicating the version pin here; re-verify both together if either changes.
