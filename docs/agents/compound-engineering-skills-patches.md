# Compound Engineering skills — local patch ledger

Every local change applied to a vendored skill from
[EveryInc/compound-engineering-plugin](https://github.com/EveryInc/compound-engineering-plugin), one row per
patch id per touched file. **All unpatched content is byte-identical to upstream commit
`67cc7dc7a11ab3724ca8e0723fcf18ee08e605de`** (plugin `3.22.4`); see
`compound-engineering-skills-manifest.json` for the per-file `upstream_blob_sha` that pins it, and the
`vendored_blob_sha` that pins what is actually on disk.

**Marker convention.** Three forms appear in this set, all carrying the same id:

- Paired, wrapping replacement text in Markdown —
  `<!-- bukerov-local-patch: <id> -->…<!-- /bukerov-local-patch: <id> -->`.
- Self-closing, single-line, with an em-dash note — `<!-- bukerov-local-patch: <id> — what was deleted -->`.
  This is the form used where upstream text was **removed**, since there is nothing left to wrap; it is the
  marker that records the removal at the exact spot it happened.
- `# bukerov-local-patch: <id> — <note>` comment lines immediately above the changed lines in shell, YAML and
  Python files.

No Python, HTML or JSON file is patched in this set. `scripts/validate-agent-governance.py` checks that
wrapping markers balance per id (`patch-marker-balance`) and that marker ids, manifest `patch_ids` and this
ledger cover each other in both directions (`provider-patch-marker-coverage`).

No translations, no stylistic rewrites, no reflowed prose: every content row below removes an **authority** —
a merge, a ready-for-review flip, a workflow rerun, a push to the default branch, a write into the governance
layer, or a permission bypass. Nothing narrows a skill's orchestration: its subagents, reviewer lanes, writer
count, parallel dispatch and repair loops are preserved exactly as upstream wrote them (see
`compound-engineering-skills-policy.md`, "Default: minimal patching").

## What the `d6ae4645 → 67cc7dc7` re-vendor did to this ledger

Upstream restructured by size: `SKILL.md` bodies were split out into `references/*.md` (`ce-babysit-pr`'s
`SKILL.md` went 90128 → 7939 bytes). **Patched text moved with it.** No patch id was created and none was
retired — all seven survive — but their file lists changed substantially, in both directions:

| Patch id | Files before | Files now | What moved |
| --- | --: | --: | --- |
| `ce-no-merge-authority` | 5 | **13** | the `stack-land` land step spread from `SKILL.md` into new `stack.md`, `envelope.md`, `settle.md`, `tick.md`, `pipeline.md`, `report.md`, `setup.md`; `ce-commit-push-pr` gained `apply-and-handoff.md` |
| `ce-no-workflow-rerun` | 1 | **3** | the rerun instruction spread from `SKILL.md` into `tick.md`; the write-gate clause into `envelope.md` |
| `ce-no-permission-bypass` | 5 | **6** | the `mode: "auto"` prose left `ce-work/SKILL.md` for `references/execution-strategy.md` |
| `ce-no-governance-file-writes` | 2 | **3** | the Discoverability Check left both `SKILL.md`s for `refresh-and-discoverability.md` / `discoverability.md` |
| `ce-no-direct-main-push` | 2 | 2 | the commit-strategy menu left `ce-compound-refresh/SKILL.md` for `references/commit.md` |
| `ce-draft-pr-only` | 3 | 3 | the `gh pr create` fence left `ce-commit-push-pr/SKILL.md` for `references/apply-and-handoff.md` |
| `ce-mode-normalize` | 27 | 27 | unchanged set; one blob re-pinned (`pr-snapshot`) |

Two `SKILL.md` files — `ce-compound-refresh/SKILL.md` and `ce-work/SKILL.md` — **stopped being patch targets
entirely**, because upstream reduced them to pointer shells that no longer contain the text the patch edits.
Re-applying the previous file list would have left dead patches on those two and, far worse, would have left
the relocated authority unpatched everywhere it moved to. Content-patched files went 15 → 25.

Rows are grouped: content patches first, then the mode-only normalization.

## Content patches (30 rows, 25 files)

### `ce-no-merge-authority` — 13 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-babysit-pr | `skills/ce-babysit-pr/SKILL.md` | `89eeb9717841b31947c2efab1c4a476c3ec28c69` | Seven edits removing `stack-land` from the compressed routing shell: the frontmatter `argument-hint` loses the posture (self-closing marker on the line after the `---` terminator, matching the previous vendored convention); the `stack-land` posture bullet — "selecting it **is** land authorization: once the bottom-most open layer is settled, `gh stack merge` it + `gh stack sync`" — is deleted; the Selection clause "land → `stack-land`" is dropped; "**Merge-readiness is never merge authorization** except under `stack-land`" becomes the unconditional sentence; the mutation-envelope exclusion "(except the caller-owned stack-land step)" is removed; the Terminal-check parenthetical "(a `stack-land` merge this run landed is a transition)" is deleted so `MERGED` is always terminal; and "`stack-land` lands the settled prefix before advancing" is dropped from the stop summary. | This is the one posture where the skill grants itself run-level land authorization and then merges. The owner performs merges — non-delegable at every depth (`CLAUDE.md`, "Non-delegable prohibitions"), and `.claude/settings.json` denies `Bash(gh pr merge *)` and `mcp__github__merge_pull_request` outright. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/envelope.md` | `e40745746da1cefefbd4396aa364b8d45ce35fb6` | **New file this pin** — carries the boundaries/authority/mutation-envelope text relocated out of `SKILL.md`. Seven edits: the "except `posture:stack-land`" carve-out on the merge-readiness boundary is removed; "Under `stack-land`, selecting that posture authorizes the prefix land step after settle" is deleted; the clause authorizing `gh stack merge … --yes --squash` + `gh stack sync` after settle is deleted; "under `stack-land` the authorized prefix merge is part of the envelope" is dropped; and the delegated-authority exclusion list loses both its `stack-land` carve-out and the "The `stack-land` merge+sync step is likewise caller-owned" sentence. | Same boundary as the `SKILL.md` row, applied where the restructure put the authoritative envelope text. Leaving it here would have restored merge authority in full while `SKILL.md` looked clean. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/stack.md` | `fab329c65089d4e7461f71344aab50f0afdb0f4d` | **New file this pin** — the single largest relocation. Nine edits: the `stack-land` row is removed from the posture table (the row that made "Selecting or handing off `posture:stack-land` **is** run-level land authorization"); the posture carrier list, the Selection clauses, the "already supplied" list and the "Once `stack-ready` or `stack-land` is in effect" sentence all lose the posture; the section heading "Layer transitions and the stack-land land step" becomes "Layer transitions"; the "Under `stack-land`, run the land step below **before** any plain advance" sentence is deleted; and **the entire `stack-land` land-step paragraph is deleted** — identify the bottom-most open settled PR, `gh stack merge <that-PR> --yes --squash`, `gh stack sync --remote <tracking-remote>`, the merge-queue re-probe and the just-landed layer transition. | The full merge recipe upstream moved out of `SKILL.md` into this new file. This is the row that would have silently restored land authority in a mechanical re-vendor. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/settle.md` | `fd51ee93cbc2be3aa1b5bb0eace319ce3be33452` | **New file this pin.** Two edits: the stop-condition preamble loses its "`stack-ready`/`stack-land`" posture pair; and the Terminal bullet loses the exception "except when this run just completed an authorized `stack-land` merge on that PR: that MERGED outcome is a layer transition … not a run-level Terminal stop", so `MERGED` is unconditionally terminal. | Keeps the settle gate from re-introducing the land step by exception. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/tick.md` | `fbc07b5f0ceed50c3da47e2576cf8dc4fbb7ce8c` | **New file this pin.** Two edits dropping the `stack-ready`/`stack-land` posture pair from the downstack-probe arm instruction and from the Terminal-check bullet. | Same boundary, applied to the relocated tick mechanics. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/pipeline.md` | `05f8bab628862bbe87299ce9f9fc5a8b0526ec04` | **New file this pin.** One edit removing the `stack-land` clause from the `mode:pipeline` success gate, which otherwise withheld pipeline success until a land+sync completed to `MERGED`. | `mode:pipeline` is the non-interactive path; a land gate here would merge without a human in the loop. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/report.md` | `e09d67682cbf24790a1efc7a6ed10ae6fb91b17c` | **New file this pin.** One edit removing `stack-land` from the run-posture list the report prints. The adjacent instruction to **print** the exact `gh stack merge <N> --yes --squash` command under `target`/`stack-ready` is deliberately left intact. | Printing the command for the owner is the intended end state of this patch id, not an oversight. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/setup.md` | `bf50a2551b6a3f453e47a85c76296666e1cc7f66` | **New file this pin.** Four edits removing `stack-land` from the posture-selection and continuation-offer text relocated out of Step 1. | Keeps posture selection from offering a posture the skill can no longer honour. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/stack-commands.md` | `686ded46c1df841f5e5876ed85c98778a5b598b9` | Byte-identical upstream at both pins; patch re-applied verbatim. Deleted the entire `## Land one prefix (only under `posture:stack-land`)` section — the merge instruction, the two-command recipe (`gh stack merge <BOTTOM_MOST_OPEN_SETTLED_PR> --yes --squash`, `gh stack sync --remote <tracking-remote>`) and the merge-queue re-probe paragraph — replaced by a self-closing marker. The `## Forbidden on managed stack members` section below, and the "print the exact merge command … do not execute it" line, are untouched. | The CLI recipe half of the removed posture. Post-check: the vendored file is byte-identical to the previously vendored copy. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/watch-loop.md` | `e95c9a75c9e9e417ec787b01bca0a92e61362a5b` | Seven edits. The three pre-existing sites survive (resume-syntax token list reduced to `posture:stack-ready`; managed-stack continuation precondition reduced to `stack-ready`; the `stack-land` land-step paragraph deleted). Upstream's restructure added four more: a new "landing under `stack-land` still requires the full settled gate" clause, the "**PR closed or merged externally**" exception, and two sites on the Step 5 text relocated here out of `SKILL.md`. | Same boundary as the `SKILL.md` row, applied to the detector mechanics so the two cannot drift apart. |
| ce-commit-push-pr | `skills/ce-commit-push-pr/SKILL.md` | `40e28da4a70e2ebce0839dd5d3e22c4c1763f979` | In `## Stack mode (opt-in)`, the handoff-posture derivation collapses to "always `posture:stack-ready`"; the `posture:stack-land` derivation is gone. | `posture:stack-land` no longer exists in the vendored `ce-babysit-pr`; handing it off would name a posture the receiving skill cannot honour. |
| ce-commit-push-pr | `skills/ce-commit-push-pr/references/apply-and-handoff.md` | `da202bfa5fcd7b93587224e8380d8d1f69032db8` | **New file this pin.** In the babysit-handoff paragraph, "include the derived posture (`posture:stack-ready` by default; `posture:stack-land` when land intent was explicit)" → "include the posture (`posture:stack-ready`)". | The handoff text relocated here from `SKILL.md`; the two bare "derived posture" mentions elsewhere in the file are left alone because, with both targets applied, the only derivable posture is `posture:stack-ready`. |
| ce-commit-push-pr | `skills/ce-commit-push-pr/references/stack-submit.md` | `3418296d517388ffa7cc6de3279932c11f570d62` | "Landing uses `gh stack merge` only (owned by babysit under `posture:stack-land`, or the user)." → "Landing uses `gh stack merge` only (owned by the user)." Matched on text rather than position, since upstream appended a new `## Ownership` section below it. The `gh pr merge …` fence above — upstream's own prohibition — is left intact. | There is no posture under which this skill set lands a PR. The owner performs merges. |

### `ce-no-workflow-rerun` — 3 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-babysit-pr | `skills/ce-babysit-pr/SKILL.md` | `89eeb9717841b31947c2efab1c4a476c3ec28c69` | The compressed CI bullet's flaky/infra branch changed from "flaky/infra → `gh run rerun <run-id> --failed -R <host>/<owner>/<repo>`" to surfacing the same host-qualified run ID as a **rerun residual for the owner**, with "**never** run `gh run rerun` yourself". | Triggering or rerunning a GitHub Actions workflow is non-delegable and needs a separate, direct user command. `.claude/settings.json` independently denies `Bash(gh run rerun *)`, so the upstream instruction could not execute here — it would fail at the hook rather than at a decision point, which is the wrong place for this boundary to show up. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/tick.md` | `fbc07b5f0ceed50c3da47e2576cf8dc4fbb7ce8c` | **New file this pin** — carries the full Flaky/infra bullet relocated out of `SKILL.md`. Same rewrite as above: the run ID and the `-R <host>/<owner>/<repo>` qualifier are still extracted, but the load-bearing rationale is rewritten to explain why the **owner** needs them, "The check stays red until the owner reruns it" is added, and `ce-debug`'s `flaky-infra` return is routed to the same branch instead of "treat as a rerun". Upstream's new `fixed-not-pushed` status in the surrounding bullet is left intact. | The instruction moved; the boundary has to move with it or it protects nothing. |
| ce-babysit-pr | `skills/ce-babysit-pr/references/envelope.md` | `e40745746da1cefefbd4396aa364b8d45ce35fb6` | **New file this pin.** The write-gate sentence "**Before any write** (rerun, or a delegated push/reply)" → "**Before any write** (a delegated push/reply)". | Removes rerun from the enumerated set of writes this skill may perform. |

### `ce-draft-pr-only` — 3 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-commit-push-pr | `skills/ce-commit-push-pr/references/apply-and-handoff.md` | `da202bfa5fcd7b93587224e8380d8d1f69032db8` | **New file this pin** — the "Applying via `gh`" fence relocated here from `SKILL.md`. `gh pr create --title "<TITLE>" --body-file "$BODY_FILE"` → `gh pr create --draft --title "<TITLE>" --body-file "$BODY_FILE"`. The adjacent `gh pr edit` line (existing PR) is byte-unchanged. No marker is added to the new `SKILL.md`: it carries zero code fences and no `gh pr create` invocation, so there is nothing left there to patch. | Marking a PR ready for review is owner-only and non-delegable; a PR created ready has already made that decision. `.claude/settings.json` denies `Bash(gh pr ready *)` and `Bash(gh pr edit * --ready*)` and gates `mcp__github__update_pull_request` to `ask`, so drafting at creation is the shape that keeps the ready-flip with the owner rather than with a hook denial. |
| ce-commit-push-pr | `skills/ce-commit-push-pr/references/gh-stack-cli.md` | `4d4f94460cda55ef88207baeb41a097b819147b4` | Byte-identical upstream at both pins; patch re-applied verbatim. `gh stack submit --auto [--open]` → `gh stack submit --auto`, and the prose documenting `--open` ("creates PRs ready for review instead of drafts, and also marks pre-existing drafts ready") gains "— never pass it: marking a PR ready for review is owner-only." The flag's documentation is kept, not deleted, so a reader still knows what it does and why it is off-limits. | `--open` both creates ready PRs and flips existing drafts to ready — two owner-only actions in one flag. |
| ce-commit-push-pr | `skills/ce-commit-push-pr/references/stack-submit.md` | `3418296d517388ffa7cc6de3279932c11f570d62` | Three edits: the heading `## Submit (ready / non-draft)` → `## Submit (draft)` with a self-closing marker recording the change; the submit fence `gh stack submit --auto --open` → `gh stack submit --auto`; and the branch condition "When no existing drafts are present **(or the user explicitly authorized opening every layer)**:" → "When no existing drafts are present:". Upstream's own "do **not** pass `--open`" paragraph and its draft-only-is-a-hard-residual sentence are left standing; the newly appended `## Ownership` section is unpatched. | Same as the `gh-stack-cli.md` row. The consequence upstream already documents becomes the normal path: stack submits always produce drafts, so babysit handoff reports a draft-only residual until the owner marks them ready. The third edit is why the condition had to move too: upstream used that alternative to select `--open`, so with the flag gone both branches submit identically and the clause named an outcome it could no longer reach — leaving it would have had an agent report layers as opened ready when only drafts were created. Found by the Q3 review of this re-vendor. |

### `ce-no-governance-file-writes` — 3 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-compound | `skills/ce-compound/SKILL.md` | `d500ad6f4fb762ccd4c11006023cefa2407d0a18` | One insert, no deletions. Into the new `## Write boundary` section, after "An instruction file is only ever edited, never created." and before "Nothing else in the tree is written:": "In this repository `CLAUDE.md` and anything under `.claude/` are governance documents and are never edit targets — report the suggested edit to the owner instead of applying it." The sentence is byte-identical to the one this patch carried at the old pin; only its host paragraph changed upstream. | Upstream's consent gate is real but wrong-shaped here: the file it would edit is the level-2 authority that authorizes the skill in the first place, and a skill amending its own governing document is the integrity problem regardless of consent in the moment. `.claude/settings.json` gates `Edit(CLAUDE.md)` and `Edit(.claude/rules/**)` to `ask` and denies `Edit(.claude/hooks/**)`; this makes the skill stop and report rather than walk into that gate. `AGENTS.md` is untouched as an edit target — this repo has none. |
| ce-compound | `skills/ce-compound/references/refresh-and-discoverability.md` | `d60617048ad75c7e57404776ca4b25e675dbf608` | **New file this pin** — the Discoverability Check relocated out of `SKILL.md`, step numbering unchanged. The carve-out is prepended at the head of step 4c, before the consent flow: "If the target is `CLAUDE.md` or anything under `.claude/`, do not edit it — report the suggested edit to the owner instead and skip the rest of this step." | The check moved; the carve-out has to move with it. |
| ce-compound-refresh | `skills/ce-compound-refresh/references/discoverability.md` | `0102f820d7ae9a6ab82aa16ac21ac94d9b4b4630` | **New file this pin** — the Discoverability Check relocated out of `SKILL.md`. Step 1 gains the byte-identical carve-out sentence: "In this repository `CLAUDE.md` and anything under `.claude/` are governance documents and are never edit targets — when the gap is there, report the suggested edit to the owner instead of editing." No marker is added to the new `SKILL.md`: its `## Discoverability Check` section names no edit target. | Same ground as the `ce-compound` rows; this skill runs the same check against the same files, so the boundary has to exist in both or it exists in neither. |

### `ce-no-direct-main-push` — 2 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-compound-refresh | `skills/ce-compound-refresh/references/commit.md` | `0957ffd516d4c9ea69e0610a6ab86b9bf1d5f39e` | **New file this pin** — the commit-strategy menu relocated out of `SKILL.md`. On the default branch the interactive option set "branch+commit+PR (recommended; specific branch name) / **commit directly to the current branch** / don't commit" loses its middle item. The clean- and dirty-feature-branch option sets are unchanged, and the non-interactive default is left untouched because it already branches on the default branch. No marker is added to the new `SKILL.md`: its `## Commit` section is a pointer and offers no menu. | On the default branch, "commit directly to the current branch" is a direct commit to `main`. Offering it as a menu item invites it; removing the option leaves branch-and-PR as the only writing path. |
| ce-debug | `skills/ce-debug/references/pipeline-mode.md` | `8c2a6c38305d195bc6bfe1ac91c0b6c29bb5e092` | Re-applied at the audited location — the Phase 3 (workspace/branch) bullet under `## Non-interactive overrides (per phase)`. Between "Commit the fix … and push." and "Never weaken, skip, or mock a failing assertion": "Never commit or push while the current branch is the default branch (`main`/`master`) — in that case stop and report, leaving the fix uncommitted for the caller." | Pipeline mode is explicitly non-interactive and "operates on the current branch — the orchestrator owns branch context". When an orchestrator (or a user) invokes it from `main`, upstream's text commits and pushes there with nothing to stop it. This is the one guard that makes the "orchestrator owns branch context" assumption safe to inherit. |

### `ce-no-permission-bypass` — 6 files

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-work | `skills/ce-work/scripts/cross-model-work.sh` | `92420343677706b52b25ecbc81265d9245d5499d` | Byte-identical upstream at both pins; patch re-applied verbatim, five hunks. **claude:** `--permission-mode bypassPermissions --tools Read,Write,Edit,Bash --allowed-tools 'Bash(*)'` → `--permission-mode acceptEdits --tools Read,Write,Edit,Bash` — the blanket `Bash(*)` grant is gone and the explicit `--tools` list stays. **codex:** `codex exec --ignore-user-config --ignore-rules --ephemeral -s workspace-write …` → `codex exec --ephemeral -s workspace-write …`. **cursor / composer / grok-cursor:** `--force --sandbox enabled --trust` → `--sandbox enabled`. | Four different ways of saying "run the peer with its policy layer off": a Claude permission-mode bypass plus an unrestricted `Bash(*)` grant, a Codex invocation that discards the operator's own config *and* rules, and three `cursor-agent` routes combining `--force` with `--trust`. Every sandbox and workspace pin is kept, so the routes still work; only the opt-outs are gone. This repo additionally sets `"disableBypassPermissionsMode": "disable"`, so the Claude route's bypass could not have taken effect here in any case. |
| ce-work | `skills/ce-work/references/execution-strategy.md` | `dfe335e061e4a23ac0e71ebe1d9d1f8fb745010c` | **New file this pin** — the subagent-dispatch note relocated out of `SKILL.md`. One word: "Do not pass `mode: \"auto\"` — it overrides user-level settings like `bypassPermissions`" → "… like `acceptEdits`". The instruction (omit `mode`; never pass `mode: "auto"`) is unchanged. `SKILL.md` now carries no marker and is verbatim upstream. | Mechanical, and stated as such: citing `bypassPermissions` as the user-level setting worth preserving is misleading in a repo that disables that mode outright, and it names the mode as a normal thing to be in. No behaviour changes. |
| ce-code-review | `skills/ce-code-review/scripts/cross-model-adversarial-review.sh` | `14b105f1cae8831c20f158876ba63923fa469470` | Byte-identical upstream at both pins; patch re-applied verbatim. Dropped `--trust` from all three `cursor-agent` adapter argv lines (`grok-cursor`, `cursor`, `composer`). `--mode ask`, `--sandbox enabled`, the workspace pin, the optional `--add-dir` and the output format are all kept. Three marker comments record the lines. | `--trust` disables the peer CLI's own workspace-trust prompt for a directory this script hands it. The review lane gains nothing from it — `--mode ask` is already read-only — and it silently widens what a peer process may do on this machine. Subtractive: no route disabled, no adapter rewritten. |
| ce-doc-review | `skills/ce-doc-review/scripts/cross-model-doc-review.sh` | `35e34099ee060bbc7a7c835800f04ffb3b6da4d3` | Blob re-pinned this round (was `c21d432e276aba3c1ed755493c8d4ecf0934697e`). The **only** upstream change is one line ~590 lines below the patched region: the `attempt_route` log gained "; full document content egresses to this provider via this route" — an egress disclosure, taken verbatim. The patched region itself is byte-identical, so the same edit re-applies in place: `--trust` dropped from the `grok-cursor`, `cursor` and `composer` routes, with `--mode ask --sandbox enabled --workspace "$PEER_WORKDIR" --output-format stream-json` kept and the same three marker comments. Post-check: the vendored file is 1028 lines (upstream 1025 + 3 marker comments) and a `--trust` search returns only those three comments. | Same rationale; this script is the doc-review lane's copy of the same adapter block. |
| ce-pov | `skills/ce-pov/scripts/cross-model-pov.sh` | `541a37ac2dd7175a22e0ed6a9841120b606a781d` | Byte-identical upstream at both pins; patch re-applied verbatim on the three `cursor-agent` routes that pin `--workspace "$READ_ROOT"`. | Same rationale; `ce-pov`'s peer routes are read-only by construction, which makes `--trust` purely a widening. |
| ce-optimize | `skills/ce-optimize/references/optimize-spec-schema.yaml` | `56da4154a48dea4ed6d5b24e4dea9892bc4c8b14` | Byte-identical upstream at both pins; patch re-applied verbatim. `codex_security` enum: removed the `yolo` value, whose inline comment documents it as `--dangerously-bypass-approvals-and-sandbox`. `full-auto` is retained and the field is otherwise unchanged. **The marker's own note was corrected this round:** it cited "SKILL.md step 5 applies whichever posture is selected", and that step moved in the restructure — it now cites `references/loop.md` § 3.2 step 5, verified at `references/loop.md:102` under the **Codex backend** heading. | A full approvals-and-sandbox bypass was one of exactly two selectable postures, and the step that applies the selected posture is live — so this was a live bypass path, not documentation. |

## Mode normalization (27 rows, 27 files)

Content is untouched in every row below — each file's `upstream_blob_sha` still matches on disk — **except**
the four cross-model scripts, whose content change is recorded in its own row above. The shared reason for all
27: vendored files are content an agent reads and then runs through an explicit interpreter (`bash
"$SKILL_DIR/scripts/…"`, `"$PY" "$SKILL_DIR/scripts/pr-snapshot" …`, `"$NODE" "$SKILL_DIR/scripts/context.mjs"`),
never a binary invoked straight off disk, so `.claude/skills/**` carries no executable bit at all
(`no-symlinks-no-exec-under-claude`). Giving the change its own id makes it mechanically checkable:
`provider-vendored-modes` fails closed on any `upstream_mode`/`vendored_mode` difference that no patch id
documents, in either direction.

The set of 27 files is unchanged from the previous pin. One blob was re-pinned: `ce-babysit-pr`'s
`scripts/pr-snapshot` moved `622e9d41515f53cef36463b0a9339f57b5ffc4db` →
`1865073d3fc04cd941d30d42abd3f6ff34228b38` (+272/-31 lines, re-audited end to end — see that skill's
`audit_ref`), and `ce-resolve-pr-feedback`'s `scripts/get-pr-comments` moved
`c1c80854cca2eb84e9857a7be0ba3e7100d20116` → `70bdcf8e2b129c3a3c78fa2ae992a8c73c09f15d` (also re-audited).
Both remain mode-only, with no content patch.

| Skill | Upstream path | Blob SHA | Local change | Reason |
| --- | --- | --- | --- | --- |
| ce-babysit-pr | `skills/ce-babysit-pr/scripts/pr-snapshot` | `1865073d3fc04cd941d30d42abd3f6ff34228b38` | Mode `100755` → `100644`; content byte-identical to upstream. Blob re-pinned this round. | Extensionless Python helper, invoked as `"$PY" "$SKILL_DIR/scripts/pr-snapshot" …`. |
| ce-brainstorm | `skills/ce-brainstorm/scripts/elevation-dispatch.sh` | `6a9d59b9c121a0edbf4cb769d035aceb00a0e3d9` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. |
| ce-brainstorm | `skills/ce-brainstorm/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-code-review | `skills/ce-code-review/scripts/cross-model-adversarial-review.sh` | `14b105f1cae8831c20f158876ba63923fa469470` | Mode `100755` → `100644`. Content **also** changed — see the `ce-no-permission-bypass` row above; the on-disk bytes are pinned by `vendored_blob_sha` `72836e0186fac607c1aa5ebaa93fa1425f6d0615`. | Shell script, invoked through `bash`. |
| ce-code-review | `skills/ce-code-review/scripts/findings-mechanics.py` | `f02bc7880b09d6ddc8681ca373181b827341fee9` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-code-review | `skills/ce-code-review/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. Byte-identical to the other five vendored copies (upstream parity test). |
| ce-code-review | `skills/ce-code-review/scripts/review-scope.py` | `1382f94c056dbc026c42f3f58b36f7ae764926b2` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-compound | `skills/ce-compound/scripts/session-history/discover-sessions.sh` | `cfc06ed997d8b29ec45e5c78bbfb5d7ff42c9729` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. |
| ce-compound | `skills/ce-compound/scripts/validate-frontmatter.py` | `8a8cf00019f2e66c69550726a3a4331c78be2904` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-compound-refresh | `skills/ce-compound-refresh/scripts/validate-frontmatter.py` | `8a8cf00019f2e66c69550726a3a4331c78be2904` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. Upstream's byte-duplicate of the `ce-compound` copy. |
| ce-doc-review | `skills/ce-doc-review/scripts/cross-model-doc-review.sh` | `35e34099ee060bbc7a7c835800f04ffb3b6da4d3` | Mode `100755` → `100644`. Content **also** changed — see the `ce-no-permission-bypass` row above; on-disk bytes pinned by that file's `vendored_blob_sha` in the manifest. Blob re-pinned this round. | Shell script, invoked through `bash`. |
| ce-doc-review | `skills/ce-doc-review/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-optimize | `skills/ce-optimize/scripts/experiment-worktree.sh` | `402b1b510004571977e755f6ace763fcf9d44668` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. |
| ce-optimize | `skills/ce-optimize/scripts/measure.sh` | `b66465ce924e2cf348cdbac06d8b04c87a588745` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. |
| ce-optimize | `skills/ce-optimize/scripts/parallel-probe.sh` | `6fe225911e1d4e0a4743eab47876e399ef12e1a8` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. |
| ce-plan | `skills/ce-plan/scripts/elevation-dispatch.sh` | `6a9d59b9c121a0edbf4cb769d035aceb00a0e3d9` | Mode `100755` → `100644`; content byte-identical. | Shell script, invoked through `bash`. Upstream's byte-duplicate of the `ce-brainstorm` copy. |
| ce-plan | `skills/ce-plan/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-pov | `skills/ce-pov/scripts/cross-model-pov.sh` | `541a37ac2dd7175a22e0ed6a9841120b606a781d` | Mode `100755` → `100644`. Content **also** changed — see the `ce-no-permission-bypass` row above; on-disk bytes pinned by that file's `vendored_blob_sha` in the manifest. | Shell script, invoked through `bash`. |
| ce-pov | `skills/ce-pov/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-resolve-pr-feedback | `skills/ce-resolve-pr-feedback/scripts/get-pr-comments` | `70bdcf8e2b129c3a3c78fa2ae992a8c73c09f15d` | Mode `100755` → `100644`; content byte-identical to upstream. Blob re-pinned this round. | Extensionless read-only GraphQL shell script, invoked as `bash "$SKILL_DIR/scripts/get-pr-comments" …`. |
| ce-resolve-pr-feedback | `skills/ce-resolve-pr-feedback/scripts/get-thread-for-comment` | `a0e2eae242116fee01b3de8a8428adf1429979d4` | Mode `100755` → `100644`; content byte-identical. | Extensionless shell script, invoked through `bash`. |
| ce-resolve-pr-feedback | `skills/ce-resolve-pr-feedback/scripts/reply-to-pr-thread` | `18390e778e150500832692b0d60841cbcb9a11c4` | Mode `100755` → `100644`; content byte-identical. | Extensionless shell script, invoked through `bash`. |
| ce-resolve-pr-feedback | `skills/ce-resolve-pr-feedback/scripts/resolve-pr-thread` | `0e40002c63bf22f1ef0606e24cdf6ef1b43a4c33` | Mode `100755` → `100644`; content byte-identical. | Extensionless shell script, invoked through `bash`. |
| ce-setup | `skills/ce-setup/scripts/check-health` | `363973b7960fba6750fb17f5cb9c85889511a6ea` | Mode `100755` → `100644`; content byte-identical. Unchanged at this pin. | Extensionless shell script; SKILL.md invokes it as `bash "$SKILL_DIR/scripts/check-health" --version VERSION`. |
| ce-work | `skills/ce-work/scripts/cross-model-work.sh` | `92420343677706b52b25ecbc81265d9245d5499d` | Mode `100755` → `100644`. Content **also** changed — see the `ce-no-permission-bypass` row above; on-disk bytes pinned by that file's `vendored_blob_sha` in the manifest. | Shell script, invoked through `bash`. |
| ce-work | `skills/ce-work/scripts/peer-job-runner.py` | `41234ae6d4cbbd6dca0dca37c87df37b344f439e` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |
| ce-work | `skills/ce-work/scripts/unit-workspace.py` | `07f35f4e03414a49d2381f17402347c3cf55b0d0` | Mode `100755` → `100644`; content byte-identical. | Python script, invoked through an explicit interpreter. |

## Not patched, on purpose

Recorded here because each was considered and deliberately left alone; a future re-vendor should not "fix"
them.

- **`scripts/context.mjs`** (13 installed copies of upstream's 15, blob
  `e0d7dae5b98d6feadb29c6f94ef37d65ff57d386`, unchanged at this pin; the other two belong to `ce-retune` and
  `ce-sweep`, both excluded) — read end to end and preserved verbatim. It shells out only to read-only `git rev-parse`,
  makes no network call and writes nothing; its `SUBAGENT_AUTHORIZATION` preamble authorizes Compound
  Engineering's own documented subagent topology, which Governance v3 explicitly assigns to the skill. See
  `compound-engineering-skills-policy.md`, "`context.mjs`: read in full, preserved deliberately", for the full
  analysis including the honest structural caveat about a bundled script that emits directives as tool output.
- **Every orchestration surface** — persona fan-out in `ce-code-review`, subagent units in `ce-work`,
  two-independent-persona promotion in `ce-doc-review`, peer-model elevation in `ce-plan` / `ce-brainstorm`,
  and `ce-babysit-pr`'s self-sustaining delegate-and-repair loop. Workflow belongs to the skill. The
  restructure this round moved a great deal of that prose into new reference files; none of it was patched.
- **`allowed-tools: Bash(gh *), Bash(git *), Read`** in `ce-resolve-pr-feedback`'s frontmatter — that key
  narrows the tool surface, so the validator's frontmatter allowlist was widened for this provider instead of
  the skill being edited.
- **`ce-babysit-pr`'s printed `gh stack merge <N> --yes --squash` line** (now in `references/report.md`,
  `references/envelope.md` and `references/stack-commands.md`) — printing the exact command for the owner to
  run is the intended end state of `ce-no-merge-authority`, not an oversight.
- **`ce-work/references/shipping-workflow.md`'s "Project-defined shipping process wins" paragraph** — new at
  this pin, and deliberately unpatched. It is routing indirection, not authority: it adds no command, the
  authority exercised is what `ce-work` already had at that step (commit, push, open a PR), it never mentions
  merge, ready-for-review, workflow rerun, force push or the default branch, and it explicitly reasserts the
  ship-handoff gate and publish rule. In this repository the only shipping process named in the active
  instructions is `docs/agents/skills-routing.md`'s `ce-commit-push-pr` — "Commit, push, open a **draft** PR"
  — i.e. the vendored, patched, draft-only skill. Patching it would narrow *routing*, which the policy
  forbids as a patch ground.
- **`ce-doc-review`'s new egress-disclosure log line** — taken verbatim; it discloses more, not less.

## Deferred finding — not patched here, owner decision

`ce-plan/references/plan-handoff.md` (Issue Creation, step 3) instructs: "Offer to persist the choice by adding
a `project_tracker: <value>` declaration to the project's root agent-instructions file (e.g., `AGENTS.md`; if
it `@`-includes another file, write to the substantive one)." In **this** repository the root
agent-instructions file is `CLAUDE.md` — exactly the target `ce-no-governance-file-writes` protects in
`ce-compound` and `ce-compound-refresh`. `ce-plan` has never been covered by that id.

It is **not introduced by this update**: the sub-bullet is byte-identical at both pins (the file's only
upstream change in this delta is an unrelated "in chat" → "on the host's user-visible chat surface" wording
fix), and it was already present unpatched in the superseded vendored copy. It is therefore a pre-existing
accepted state, not a re-vendor blocker, and `Edit(CLAUDE.md)` is independently gated to `ask` in
`.claude/settings.json`. Patching it is a separate owner decision; the minimal edit would be:

> - Do not write the choice into the project's agent-instructions file. Report the suggested
>   `project_tracker: <value>` line (lowercase tracker key — `github`, `linear`, `jira`, …) to the owner and
>   let them add it themselves.
