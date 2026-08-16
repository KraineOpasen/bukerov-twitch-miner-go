# Builder.io skills — local patch ledger

**No file in this provider needed a content patch.** All 15 vendored files — 10 upstream-origin
(five `SKILL.md`, five `README.md`) plus 5 verbatim `LICENSE` copies — are byte-identical to upstream
commit `0ecfb56f3bf78b9d957246789379f3f78e2f85ec` in
[BuilderIO/skills](https://github.com/BuilderIO/skills). The manifest records `locally_modified: false`
for every file and every `upstream_blob_sha` equals its `vendored_blob_sha`.

Verified two ways while writing this ledger: `diff -u` of each vendored file against the pinned
read-only clone (all 15 diffs empty), and `git hash-object` on both sides of each pair. It is also
verified mechanically by `scripts/validate-agent-governance.py` with `GOVERNANCE_UPSTREAM_DIR_BUILDERIO`
set, which compares each unmodified file byte-for-byte against the clone rather than only against the
recorded hash. `provider-file-hashes` fails closed on any on-disk edit that is not accompanied by a
deliberate `vendored_blob_sha` bump, so this table cannot silently go stale.

**Marker convention.** No `bukerov-local-patch` marker appears anywhere in this set, because no vendored
file is modified. Were one to be added, the conventions are:
`<!-- bukerov-local-patch: <id> --> … <!-- /bukerov-local-patch: <id> -->` for a wrapped region in
Markdown or HTML, `<!-- bukerov-local-patch: <id> — <note> -->` for a standalone one-line annotation,
`# bukerov-local-patch: <id> — <note>` in Python, shell and YAML, and `// bukerov-local-patch: <id> —
<note>` in JavaScript. `provider-patch-marker-coverage` checks the correspondence in both directions:
every marker id found in a file must appear both in this ledger and in some `files[].patch_ids`, and
every id named in the manifest must appear in this ledger.

The rows below are therefore **not** content modifications. They record the one way the vendored tree
differs from the pinned upstream directories: four files that exist upstream and were deliberately not
copied. A deletion has no vendored bytes to mark and no manifest `files[]` entry to attach a
`patch_ids` list to — it shows up in the manifest only as an absence — so the ledger is where it is
recorded. The "Blob SHA" column for these rows is each omitted file's `upstream_blob_sha`, read from
the pinned clone with `git ls-tree -r`, so the exact object that was declined is identifiable.

| Skill | Upstream path | Blob SHA | Local change | Reason | Rule |
| --- | --- | --- | --- | --- | --- |
| agent-watchdog | `skills/agent-watchdog/agents/openai.yaml` | `860b5a4a99d5357cec7af0cfc4a597afc0c4f733` | Not vendored. The 4-line upstream file is a Codex/OpenAI marketplace `interface:` block — `display_name: "Agent Watchdog"`, `short_description: "Audit another agent session and fix gaps."`, `default_prompt: "Use $agent-watchdog to watch or audit an agent session, then report or fix gaps."` — with no instruction content. The vendored skill directory contains `SKILL.md`, `README.md` and `LICENSE` only. | Marketplace interface metadata for a different agent runtime, not an agent definition: Claude Code reads `SKILL.md`, and no `SKILL.md` in this provider references its `agents/` sidecar, so the file is outside the dependency closure. `openai.yaml` is additionally on this repo's forbidden-vendored-filename list (`FORBIDDEN_VENDOR_NAMES` in `scripts/validate-agent-governance.py`), enforced by `no-forbidden-vendor-files` — copying it would fail the validator. | `bio-openai-yaml-excluded` |
| plan-arbiter | `skills/plan-arbiter/agents/openai.yaml` | `1de2495b8a73aa774a604bce6abd0b55175575ef` | Not vendored. Same 4-line `interface:` shape — `display_name: "Plan Arbiter"`, `short_description: "Compare competing agent plans and pick one."`, `default_prompt: "Use $plan-arbiter to compare competing plans, cross-review them, and choose an execution plan."` | Same as above. | `bio-openai-yaml-excluded` |
| plow-ahead | `skills/plow-ahead/agents/openai.yaml` | `084303e055bf3a2cbde122a96dcc0f616452609a` | Not vendored. Same 4-line `interface:` shape — `display_name: "Plow Ahead"`, `short_description: "Proceed autonomously through ordinary ambiguity."`, `default_prompt: "Use $plow-ahead to keep working through ordinary ambiguity, make reasonable assumptions, validate, and recap decisions clearly."` | Same as above. | `bio-openai-yaml-excluded` |
| read-the-damn-docs | `skills/read-the-damn-docs/agents/openai.yaml` | `646f18352140ea0abfd6039c7512db058ac9ec52` | Not vendored. Same 4-line `interface:` shape — `display_name: "Read The Damn Docs"`, `short_description: "Make agents web-search docs before guessing."`, `default_prompt: "Use $read-the-damn-docs to web-search for official docs before implementation."` | Same as above. | `bio-openai-yaml-excluded` |

`efficient-frontier` has no row: it is the one installed skill that ships no `agents/` directory
upstream, so nothing was declined for it. A fifth `agents/openai.yaml` exists at
`.agents/skills/adding-a-skill/agents/openai.yaml` (blob
`726cb91aba0b1ae0c83ad63adef35c9738f7cc6b`); that skill is `EXCLUDE`, so its sidecar is out of scope
here rather than being a declined file.

## Reserved id: `bio-mode-normalize`

Zero rows. Every vendored file's mode is normalized to `100644` and no executable bit exists anywhere
under `.claude/skills/**`, but this provider had nothing to normalize: **every one of the 55 blobs in
`BuilderIO/skills` at this pin is already `100644`** — verified directly, `git ls-tree -r HEAD` over the
whole tree yields exactly one distinct mode, `skills/` and root scaffolding alike.

The id is named here for two reasons: so a future re-vendor that pulls in a `100755` file has an
established id rather than inventing one, and so a reader who meets `anthropic-mode-normalize` or a
sibling in another provider's ledger does not assume it was omitted here by mistake.
`provider-vendored-modes` fails closed on any undocumented `upstream_mode`/`vendored_mode` difference in
either direction, so the normalization cannot be applied silently later.

## Why the correct number of content patches is zero

Recorded because a short ledger invites the question, and the answer is a finding rather than an
omission.

The five installed skills are dependency-free prose: no scripts, no MCP servers, no CLIs, no network
calls, no writes into the working tree that the host's own permission layer does not already gate. The
audit lane scanned the provider's whole skill tree for the non-delegable operations — merge,
ready-for-review, auto-merge, force push, direct push to `main`, workflow trigger or rerun — and found
**zero hits anywhere in the provider**, installed or not. Two of the installed five state the relevant
boundaries themselves, unprompted and in upstream's own words: `plow-ahead/SKILL.md:42` lists "an
explicit branch operation, history rewrite, force push, or deletion that the user did not directly
request" among its Stop Conditions, and `agent-watchdog/SKILL.md:79-80` says "do not move branches
unless explicitly asked for that branch operation". There was no authority boundary left to narrow.

What was deliberately **not** patched is at least as important as what was. Four of the five skills are
orchestration methodologies — `efficient-frontier` delegates work to cheaper subagents,
`plow-ahead/SKILL.md:30-31` instructs "Use subagents for independent research, implementation, or
verification when parallel work can reduce idle time or improve coverage" with no cap on fan-out,
`plan-arbiter` cross-reviews rival plans produced by other agents, and `agent-watchdog` runs an
audit-then-repair loop that polls a running session. Under Governance v3 every one of those is the
skill's own engineering workflow, which the project does not override
(`docs/adr/0002-governance-v3-skill-native-orchestration.md`, `docs/agents/agent-orchestration.md`).
The patch test is **authority**, not agent topology, writer count, or repair loops. None of these
crosses an authority line, so none was touched.

See `docs/agents/builderio-skills-policy.md` for the full policy, the three held `visual-*` skills, and
the separate `BuilderIO/builder-agent-skills` audit record.
