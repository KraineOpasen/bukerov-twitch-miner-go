# GitHub awesome-copilot skills — local patch ledger

**This provider has no local patches.** All 37 vendored files — 32 upstream-origin plus 5 verbatim
`LICENSE` copies — are byte-identical to upstream commit
`318066d2213b510e89b500ed0d53506c54093ddc`. The manifest records
`locally_modified: false` for every file, and every `upstream_blob_sha` equals its
`vendored_blob_sha`.

The pin moved from `a80885b76044550770f60f360f8a0e5ae3524a31` on 2026-08-19 and **this ledger's
substance did not change with it**: the two commits carry identical bytes for all five installed
skills, so every statement below was true at the old pin and remains true at the new one. Only the
commit this file names as the provenance baseline moved.

Verified by diffing the whole vendored set against a read-only clone at that commit, and again by
`scripts/validate-agent-governance.py` with `GOVERNANCE_UPSTREAM_DIR_AWESOME_COPILOT` set, which
compares each unmodified file byte-for-byte against the clone rather than only against the recorded
hash.

Two limits of that check are worth recording, because a green run is easy to over-read.

First, it covers **32 of the 37 files, not all 37**: the five `LICENSE` copies carry
`origin: "local"`, and `provider_file_hash_details` `continue`s on that before it reaches the
clone comparison — so their bytes are checked against the recorded `vendored_blob_sha` but never
against upstream. Their identity to upstream's root `LICENSE` blob
(`89bc5e962c9944cdb050887062afdaaf89be504a`) is established by hand, and must be re-established by
hand on every re-vendor.

Second, the variable must point at a **working-tree checkout**, never a bare or mirror clone.
`provider_file_hash_details` guards its comparison with `os.path.isfile(<clone>/<upstream_path>)`
and has no else branch: against a bare clone no such file exists, every comparison is skipped
silently, and the run still prints both the "verified against an upstream clone" banner and
`[PASS]`. A bare clone therefore yields a green result that proves nothing. Note also what the
strict mode does *not* prove: it compares **bytes, not the pin**. The clone's `HEAD` is never
resolved and `upstream_commit` is only shape-checked as 40 hex characters, so a pass is compatible
with the clone sitting at any commit whose installed subtrees match — including the superseded one.
Whoever moves the pin must establish the commit out of band; this check will not do it.

`provider-file-hashes` fails closed on any on-disk edit that is not accompanied by a
deliberate `vendored_blob_sha` bump, so this table cannot silently go stale.

| Skill | Upstream path | Blob SHA | Local change | Reason | Rule |
| --- | --- | --- | --- | --- | --- |
| _(none)_ | — | — | No file in this provider's installed set is modified. | — | — |

## A patch that was made and then withdrawn

This is recorded because the reasoning is reusable, not because anything remains changed.

During vendoring, `relative-links-resolve` reported nine dangling links in
`threat-model-analyst/references/output-formats.md`. They were Markdown links to documents the skill
*generates in the target repo* (`0-assessment.md`, `2-stride-analysis.md`, …) — never to files
vendored with the skill. A patch (`ghac-generated-output-links`) rendered them as code spans, which
cleared the finding.

Writing this ledger surfaced the actual cause: the defect was in the **checker**, not the content.
`strip_code_fences()` paired fences with a regex (```` ```.*?``` ```` under DOTALL), which cannot
express CommonMark's rule that a fence closes only on the same character, at least as long as the
opener. `output-formats.md` contains a nested four-backtick fence — the standard way to show a
fenced block inside a fenced block, and exactly what a skill documenting its own output format does.
The regex mis-paired those fences, so the specimen tables fell outside any recognised fenced region
and their example links were checked as if they were real.

The validator was fixed instead: a line-based, fence-length-aware stripper honouring same-character
closing, closer length, a bare closer, and the rule that an opening backtick fence may not carry a
backtick in its info string. Self-test fixture `P16` covers all four, and each was
mutation-verified. With the checker correct, `relative-links-resolve` passes against the
**unmodified** upstream file, so the patch was reverted and `output-formats.md` restored
byte-identical to the pin.

The general rule this illustrates, and the reason it is written down: a validator finding that can
only be cleared by editing correct upstream prose should be treated as a suspected defect in the
validator first. The pressure to edit upstream to satisfy a checker is exactly what these vendoring
policies exist to remove.

## Reserved id: `ghac-mode-normalize`

Every vendored file's mode is normalized to `100644`; no executable bit exists anywhere under
`.claude/skills/**`. This provider records **no row** for it, because all 32 upstream-origin files in
the installed set are already `100644` at the pin — there was nothing to normalize. The id is named
here so a future re-vendor that pulls in a `100755` script has an established id rather than
inventing one, and so a reader who meets it in another provider's ledger does not assume it was
omitted here by mistake. `provider-vendored-modes` fails closed on any undocumented
`upstream_mode`/`vendored_mode` difference in either direction, so the normalization cannot be
applied silently later.

## Marker convention

No marker appears in any file in this set, because no file is patched. Were one to be added, the
conventions are: `<!-- bukerov-local-patch: <id> --> … <!-- /bukerov-local-patch: <id> -->` for a
wrapped region in Markdown or HTML, `<!-- bukerov-local-patch: <id> — <note> -->` for a standalone
one-line annotation, `# bukerov-local-patch: <id> — <note>` in Python, shell and YAML, and
`// bukerov-local-patch: <id> — <note>` in JavaScript. `provider-patch-marker-coverage` checks the
correspondence in both directions: every marker id found in a file must appear both in this ledger
and in some file's `files[].patch_ids`, and every id named in the manifest must appear in this
ledger.
