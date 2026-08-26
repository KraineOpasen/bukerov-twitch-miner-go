# Builder.io skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of [BuilderIO/skills](https://github.com/BuilderIO/skills)
into `.claude/skills/` instead of installing it as a live Claude Code plugin or through the provider's own
`npx @agent-native/skills@latest add` flow. This document is the policy for what's installed, why, how it's
patched, and how to update it. See also `docs/agents/builderio-skills-manifest.json` (machine-readable
inventory, file-level) and `docs/agents/builderio-skills-patches.md` (per-patch ledger).

This is one of six independent vendored sets (`mattpocock`, `anthropic`, `compound-engineering`,
`trailofbits`, `awesome-copilot`, `builderio`); each has its own upstream, manifest and ledger, and no two may
claim the same directory under `.claude/skills/` — enforced by `scripts/validate-agent-governance.py`'s
`manifest-ownership-partition` check.

This policy also owns a **second** record: `BuilderIO/builder-agent-skills`, a different repository by the
same vendor that was audited in the same round and from which **nothing was vendored**. It has no manifest of
its own precisely because no file from it is installed; its audit findings live in
"Second repository audited: `BuilderIO/builder-agent-skills`" below so the work is not repeated blind.

## Upstream

- Repo: `https://github.com/BuilderIO/skills`
- Reviewed commit: `0ecfb56f3bf78b9d957246789379f3f78e2f85ec` (authored 2026-08-13, *"chore: update Agent
  Native exported skills (#59)"*)
- Reviewed tree SHAs (per vendored skill directory): `agent-watchdog`
  `016143fa2a8f50acd62eb00b0e5b3e211a48cc39`, `efficient-frontier`
  `b532256b9a1eca03c7925582baf419c9ab14cc71`, `plan-arbiter` `4ccb0322db17cd2a375fcfc906dbbdbc2f6920ea`,
  `plow-ahead` `3fa79c34fe2ef7798c9be61147fb00c2b990b188`, `read-the-damn-docs`
  `bdcaa95189bb3fad84fccb0e71ae02336e7ba85d`
- Current upstream HEAD at review time: same SHA (**drift: none**)
- Corpus size at the pin: **13 skills** — 12 under `skills/` plus one under `.agents/skills/`
  (`adding-a-skill`) — of which **5** are installed, 3 are `HOLD` and 5 are `EXCLUDE`.

Two provenance facts shaped the review and are worth stating up front.

**The review clone is a depth-1 shallow clone with no configured remote** (`git remote -v` is empty,
`git rev-list --count HEAD` is 1). "Current upstream HEAD: same SHA" is therefore what the audit lane recorded
at review time; it was not re-fetched from GitHub while this document was written. Provenance for this pin
rests on the clone's contents, hashed file by file, not on a live comparison against `main`.

**Four of the 13 skills are generated exports of a private sibling repository, not content authored here.**
`scripts/sync-agent-native-skills.mjs` copies `visual-plan`, `visual-recap` and `visual-edit` out of
`../agent-native/framework` (resolved via `AGENT_NATIVE_SKILLS_SOURCE`, line 20), and extracts `rewind`'s
`SKILL.md` from a TypeScript template literal at
`packages/core/src/cli/skills-content/rewind-skill.ts` (line 96). None of the five installed skills is in that
generated set, which matters: the installed slice is hand-authored content that lives in the repository being
pinned, so the pin actually pins it. The two provider scripts are outside every skill's dependency closure and
were read end to end by the audit lane; nothing is vendored from `scripts/`.

## Installation model

**Project-local vendored copy**, not a live install. Each skill's files are copied verbatim into
`.claude/skills/<name>/` at review time, then minimally patched (see below), and every file's mode is
normalized to `100644` — no executable bits anywhere under `.claude/skills/**`
(`no-symlinks-no-exec-under-claude`). `automatic_updates: false` — nothing about this installation re-fetches
or re-syncs from upstream on its own. A human, or an explicitly-contracted agent task, must re-run the review
process to move the pin.

The mode-normalization rule binds this provider like every other, and it produced **no rows here**: every one
of the 55 blobs in the upstream repository at this pin is already `100644` (verified directly —
`git ls-tree -r HEAD` yields exactly one distinct mode across the whole tree, `skills/` and root scaffolding
alike). Other providers in this repo do carry a `*-mode-normalize` id for the `100755` case; this one reserves
the id `bio-mode-normalize` without using it. `provider-vendored-modes` still fails closed in both directions,
so a future re-vendor that pulls in an executable file cannot record the normalization silently.

The vendored set is **15 files**: 10 upstream-origin (five `SKILL.md`, five `README.md`) plus five
local-origin `LICENSE` copies, one per skill directory (see "License & attribution"). **The installed slice
ships no scripts of any kind** — no Python, no shell, no JavaScript, no HTML, no JSON. That is why
`scripts_audited` is `null` for all five skills rather than `true`: there was no executable content to audit,
not an audit that was skipped.

One upstream file per skill was deliberately **not** copied: `agents/openai.yaml`, present in four of the five
installed skills (`efficient-frontier` ships none). That omission is a vendoring-boundary decision, recorded
as `bio-openai-yaml-excluded` in the ledger — see "Local patches summary".

## Installed: 5 skills

All five are single-`SKILL.md` skills with no `references/`, no `scripts/`, no `assets/` and no external
dependency of any kind. This is the lowest-risk vendoring batch of any provider in this repo. Routing entries
for all five already exist in `docs/agents/skills-routing.md`.

- **`agent-watchdog`** — audits what *another* agent actually did, against the user's reconstructed request:
  resolve the artifact (session ID, transcript, PR, branch, CI run, pasted summary), rebuild the contract,
  inspect evidence rather than the other agent's summary, and classify each issue as gap / bug / verification
  miss / scope drift / no issue. Distinct from the installed `code-review`, which reviews a diff against
  standards and spec — this reviews a *session* against what was asked. It is self-limiting where it matters:
  "If authority is unclear, default to audit-only and say what you would fix" (`SKILL.md:25`) and, in fix
  mode, "do not move branches unless explicitly asked for that branch operation" (`SKILL.md:79-80`).
- **`efficient-frontier`** — cost-tiered orchestration: keep architecture, prioritization, ambiguity
  resolution, risk and final review on the expensive model; delegate research scans, repository inventory,
  docs extraction, log reduction, test-failure clustering and mechanical edits to cheaper subagents, with a
  handoff-packet format and a vet-before-trusting review step. Installed in preference to its twin
  `efficient-fable` (see "Excluded / Held").
- **`plan-arbiter`** — turns two or more competing plans into one execution handoff: normalize each plan into
  comparable claims, cross-review them against each other and the real codebase, then pick a winner, merge a
  hybrid, or send them back — with verification gates and rejected alternatives recorded. No installed skill
  covers this: `grilling` stress-tests one plan and `ce-plan`/`implement` produce or execute one.
- **`plow-ahead`** — an explicitly user-triggered autonomy posture: convert ordinary ambiguity into stated
  assumptions, keep moving, validate as you go, and end with a recap that makes the decisions auditable
  (goal / key decisions / changes / validation / remaining risk). Its own Stop Conditions already name the
  boundaries this project cares about — credentials and private data unavailable, anything "destructive,
  irreversible, or production-mutating", and "an explicit branch operation, history rewrite, force push, or
  deletion that the user did not directly request" (`SKILL.md:39-43`).
- **`read-the-damn-docs`** — a pre-implementation gate that forces a web search for current primary
  documentation before answering from memory, aimed at third-party APIs, SDKs, CLIs, version drift and
  high-stakes auth/billing/migration behavior. Different in kind from the installed `research` skill, which
  is a delegated investigation that produces a Markdown file in the repo; this one is a reflex applied inside
  ordinary work.

## Excluded / Held

Eight candidates were read in full and rejected: **5 `EXCLUDE`, 3 `HOLD`**. Each carries its own reason in
`builderio-skills-manifest.json`'s `excluded_skills[]`; summarized:

- **`efficient-fable`** — a true duplicate of the installed `efficient-frontier`. Same five-step delegation
  pattern, same handoff-packet fields, same stop conditions, same four scenarios, same vet-before-trusting
  loop; `efficient-frontier`'s own frontmatter says "Apply the same orchestration as `/efficient-fable` to any
  high-cost frontier model". The differences are branding to one model plus a Claims section
  ("up to 3-5x more cost-efficient and 2-4x faster", "Good launch copy") — marketing, not methodology.
  Installing both would put two near-identical descriptions in the trigger space for no capability gain.
- **`adding-a-skill`** — repo-internal tooling for `BuilderIO/skills` itself ("Use in the BuilderIO/skills
  repo whenever adding, updating, publishing, documenting, validating, or wiring a public skill"), with a
  broken dependency closure at this pin: nine of its file references point into the absent private sibling
  `../agent-native/framework/**`, and its validation step hardcodes a foreign absolute path from a specific
  developer's laptop (`SKILL.md:51`). It also duplicates the vendored `skill-creator-anthropic` and the Claude
  Code built-in.
- **`quick-recap`** — a response-formatting convention (end every response with a 🟢/🟡/🔴 status line under
  100 characters) with no capability content. Independently, its "Installer Behavior" section instructs the
  agent to write a managed block into `AGENTS.md` / `CLAUDE.md`, which collides head-on with the standing rule
  that no agent message authorizes changing `CLAUDE.md`. Excluding is cleaner than patching a skill whose
  remaining content is four emoji rules.
- **`rewind`** — a retrieval front end for Clips Rewind screen-recording memory, requiring "the signed Clips
  Desktop app on macOS plus a local agent connection". This environment is Linux, so the prerequisite can
  never be satisfied. Every operation is a Screen Memory MCP tool with no offline mode.
- **`stay-within-limits`** — agent-billing-budget infrastructure (pause when the usage window hits 95%), not
  a project capability, and overlapping ground the installed `handoff` and `efficient-frontier` already hold.
  Its only concrete mechanism is `npx -y ccusage@latest blocks --active --json` (`SKILL.md:30`) — an unpinned
  third-party CLI auto-accepted with `-y` on a recurring loop. It becomes `HOLD` if the owner ever wants
  budget throttling, unblocked by pinning `ccusage` to an exact version.

### Held, not excluded

The three `visual-*` skills are `HOLD`: real, non-duplicated capability, blocked by something specific.
None is installed, and none becomes installed by anyone deciding it "seems fine now" — an unblock is a
re-vendor through the update procedure below.

- **`visual-plan`.** *What it would add:* a structured, shareable, commentable plan artifact — data-model,
  API-endpoint, file-tree and wireframe blocks — as the planning deliverable instead of chat prose.
  *What blocks it:* the skill's own hard rule is that "The deliverable is ALWAYS a structured Agent-Native
  Plan, not a chat-only plan", and `references/connection.md` adds "If the tools are still missing after
  discovery, do NOT fall back to inline output". Every authoring verb (`get-plan-blocks`, `create-visual-plan`,
  `create-ui-plan`, `update-visual-plan`, `get-plan-feedback`) is a tool on the hosted connector
  `https://plan.agent-native.com/mcp`, which this project does not have. The documented escape hatch,
  Local-Files Privacy Mode, is **not** MCP-free: it still needs `npx @agent-native/core@latest plan blocks`
  for the block catalogue and `plan local check` / `plan local serve` / `plan local verify` for every
  preview and validation step, and its served preview "opens the hosted Plan UI but reads from the localhost
  bridge" — so `plan.agent-native.com` must be reachable even there. *What would unblock it:* pin
  `@agent-native/core` to an exact version **and** either provision the hosted Plan connector or run a
  loopback Plan app (`--app-url http://localhost:8096`).
- **`visual-recap`.** *What it would add:* a diff-grounded review artifact, and the methodology is genuinely
  strong — data-model / api-endpoint / file-tree / split-diff blocks derived mechanically from the actual
  changed lines, under an explicit Grounding Rule ("A confidently wrong recap is dangerous in a review
  context") and a Security section forbidding secret transcription that mirrors this repo's own redaction
  rule. *What blocks it:* the same mandatory connector, stated even more absolutely — "The deliverable is
  ALWAYS a published Agent-Native Plan, created with `create-visual-recap` on the Plan MCP connector — NEVER
  inline chat content", and "an inline summary is not a degraded recap, it is the thing a recap replaces."
  Local-files mode routes through the same unpinned CLI. A secondary blocker: `SKILL.md:226` and `:229` point
  at `../visual-plan/references/canvas.md`, outside its own directory, so vendoring it alone would leave a
  dangling link at the exact place the skill says to read that file. *What would unblock it:* the same as
  `visual-plan`, plus vendoring the two skills together or relocating `canvas.md`.
- **`visual-edit`.** *What it would add:* a multi-route, multi-viewport review canvas over a running
  localhost app. *What blocks it:* the mandatory hosted Design connector **and** an entry point that does not
  exist outside Builder's own monorepo — the single documented entry action is
  `pnpm action open-visual-edit '{...}'`, a workspace script of the private `agent-native` repo that no
  consumer of this vendored skill can run. The bridge additionally needs unpinned
  `npx @agent-native/core@latest design connect …`, run as a detached daemon on fixed port `127.0.0.1:7331`,
  and the SKILL.md itself warns that the alternate install path "installs exported instructions only, with no
  MCP connector registration". *What would unblock it:* pin `@agent-native/core`, obtain the hosted Design
  connector, and publish a consumer-runnable equivalent of `pnpm action open-visual-edit`.

**A fit caveat that survives even an unblock**, recorded so nobody reopens this expecting a different answer.
The wireframe vocabulary these skills teach is proprietary to the Plan renderer — `--wf-ink` / `--wf-line` /
`--wf-paper` / `--wf-accent` tokens, `.wf-card` / `.wf-pill` helpers, a rough.js sketch overlay — and
`references/wireframe.md` explicitly bans this repo's own styling system inside wireframes: "Never use
host/Tailwind theme classes in wireframe HTML. Classes such as `bg-white`, `bg-zinc-50`, `bg-slate-950` … leak
the host app's CSS into the mockup". A `visual-plan` wireframe is therefore a review surface, not markup that
can be lifted into `internal/web/templates/*.html`; it cannot exercise HTMX partial swaps or ApexCharts; and
`visual-edit`'s source-writeback half is React/TSX-only (`positionPrecision`, `_debugSource`, Vite/Next
dev-server coordinates), which has no analogue in Go `html/template`. For Dashboard Stage 1 the installed
`frontend-design`, `dataviz`, `webapp-testing` and `prototype` cover this ground with no external dependency.
What the `visual-*` set would add is a hosted, annotatable review-and-comment surface for stakeholders — real
value, strictly additive, strictly blocked.

## Invocation modes

All five installed skills are **model-invoked**, exactly as upstream ships them. None is renamed
(`renamed_from: null` throughout), none carries `disable-model-invocation`, and each uses exactly two
frontmatter keys, `name` and `description`.

That matters for one forward-looking reason. Four of the non-installed candidates (`rewind`, `visual-edit`,
`visual-plan`, `visual-recap`) carry a third top-level key, `metadata` (nested `visibility: exported`). This
provider's `extra_frontmatter_keys` allowlist in the validator is deliberately **empty**, so if a future pin
adds `metadata` to an installed skill the frontmatter check fails rather than silently accepting it. For
comparison, the provider's own linter (`scripts/check-skill-frontmatter.mjs`) allows `allowed-tools`,
`description`, `license`, `metadata` and `name`.

## License & attribution

All five vendored skills are **MIT**, `Copyright (c) 2026 Builder.io`. MIT requires that "the above copyright
notice and this permission notice shall be included in all copies or substantial portions of the Software", so
upstream's root `LICENSE` (blob `dafdc4ad5ab52b93ffa5f677f33a45ad2944365e`) is copied **verbatim into every
vendored skill directory** — `.claude/skills/<name>/LICENSE`, five identical copies, each hash-verified in the
manifest. `provider-license-files` enforces the per-skill layout.

The audit lane recommended the opposite placement — "copy the root LICENSE exactly once into the provider's
governance area … to avoid five duplicate MIT copies under `.claude/skills/**`". The vendoring step did not
follow that recommendation, and the deciding evidence is mechanical: this provider's registry entry in
`scripts/validate-agent-governance.py` declares
`"license": {"spdx": "MIT", "layout": "per-skill", "filename": "LICENSE"}`, matching `anthropic`,
`compound-engineering`, `trailofbits` and `awesome-copilot`. A single governance-area copy would fail
`provider-license-files`, and diverging from the house layout for one provider is worse than five identical
21-line files. The lane's substantive point — that the notice must not be dropped — is satisfied either way.

- `.claude/skills/LICENSE` is the *Matt Pocock* set's shared MIT notice and does **not** cover this set. Two
  MIT upstreams, two separate attributions.
- MIT imposes no Apache-§4(b)-style "mark your modified files" obligation. The marker-plus-ledger convention
  is applied here anyway, for reviewability and because `provider-patch-marker-coverage` requires every
  in-file marker id to appear both in this provider's ledger and in some `files[].patch_ids`. No marker
  currently exists in this set, because no file is patched.
- This repository is **GPL-3.0** (root `LICENSE`). MIT is one-way compatible into a GPLv3 work — MIT code may
  be included in a GPLv3-licensed project, the reverse is not true — and the MIT notice must still accompany
  the copies, which is what the per-skill `LICENSE` files do.

## Local patches summary

**No file in this provider needed a content patch.** All 15 vendored files — 10 upstream-origin plus 5
verbatim `LICENSE` copies — are byte-identical to upstream commit
`0ecfb56f3bf78b9d957246789379f3f78e2f85ec`. The manifest records `locally_modified: false` for every file,
every `upstream_blob_sha` equals its `vendored_blob_sha`, and this was re-verified while writing the ledger by
`diff -u` of each vendored file against the pinned read-only clone (all 15 diffs empty) and by
`git hash-object` on both sides.

That is the correct outcome, not a shortcut. These are five dependency-free prose skills whose own text
already stops short of everything this project's governance protects: the audit lane scanned all of the
provider's skill-tree files for merge / ready-for-review / auto-merge / force push / direct-`main` push /
workflow trigger or rerun and found **zero hits** anywhere in the provider, and two of the installed five go
further and align with the repo invariants unprompted (`plow-ahead:42`, `agent-watchdog:79-80`, both quoted
above). There was nothing to narrow.

One vendoring-boundary decision **is** recorded, because it changes what is on disk relative to upstream:

### `bio-openai-yaml-excluded`

Four of the five installed skills ship an `agents/openai.yaml` sidecar upstream (`agent-watchdog`,
`plan-arbiter`, `plow-ahead`, `read-the-damn-docs`; `efficient-frontier` ships none). None was copied. Each is
a four-line Codex/OpenAI marketplace **interface** descriptor — an `interface:` block with `display_name`,
`short_description` and `default_prompt` — not an agent definition, not referenced by any `SKILL.md`, and not
read by Claude Code. `openai.yaml` is additionally on this repo's forbidden-vendored-filename list
(`FORBIDDEN_VENDOR_NAMES` in `scripts/validate-agent-governance.py`, enforced by
`no-forbidden-vendor-files`), so copying it would fail the validator outright.

This is a deletion relative to upstream rather than a modification of vendored bytes, which is why it has a
ledger id but no in-file marker and no `files[].patch_ids` entry — there is no vendored file to mark. Full
rows, with the upstream blob SHA of each omitted file, are in
`docs/agents/builderio-skills-patches.md`.

### Reserved id: `bio-mode-normalize`

Zero rows. Every blob in the upstream repository at this pin is already `100644`, so the standing
mode-normalization rule had nothing to normalize. The id is named so a future re-vendor that pulls in an
executable file has an established id rather than inventing one. See "Installation model".

### Default: minimal patching

Under Governance v3 (`docs/adr/0002-canonical-governance-v3.md`), skills are preserved as
close to their authors' intent as practical. **Do not patch a skill merely because it uses subagents, several
writers, reviewers/critics, parallel analysis, iterative fixes, or its own handoff/orchestration pattern.**
That is engineering workflow, and workflow belongs to the skill (see `GOVERNANCE_V3.md` §10).
Patch only for concrete project incompatibility, a broken dependency, license/provenance necessity, or a
genuine authority/integrity boundary.

This provider is the strongest illustration of the rule in the whole repo, because **orchestration is
literally what four of these five skills are about**, and not one byte of it was touched:

- `efficient-frontier` is an explicit multi-agent delegation methodology — cheap subagents for research,
  repository inventory, browser/testing passes, log reduction and mechanical edits, with the expensive model
  retained for synthesis and final review. Preserved whole.
- `plow-ahead` instructs "Use subagents for independent research, implementation, or verification when
  parallel work can reduce idle time or improve coverage" (`SKILL.md:30-31`) — unbounded fan-out, no cap.
  Preserved whole; no `agent_cap` clause was bolted on.
- `plan-arbiter` runs a cross-review-then-decide loop over rival plans produced by *other* agents.
  Preserved whole.
- `agent-watchdog` runs an audit-then-repair loop, including polling a still-running session until it reaches
  a terminal state. Preserved whole.

Each of those would have been a tempting patch target under Governance v2's orchestration rules. Under v3 the
test is **authority**, not topology, and none of them crosses an authority line — so the correct number of
patches is zero.

## Governance precedence

Vendored skills sit **below** this project's own policy **on authority**. The authority chain has exactly four
levels (see `GOVERNANCE_V3.md` §1 and §3, whose assigned positions this repo-native chain elaborates),
narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched, this set alongside every other.
4. **Generic model behavior** — fallback only.

Unpatched upstream text is **not** a fifth tier below the patches: a vendored skill's instructions are
whatever its vendored bytes say, patched and unpatched alike, all together at level 3. Where a local patch and
the upstream text around it disagree, the patch wins — that is what patching means, and it is resolved inside
level 3. A skill instruction never overrides a `.claude/rules/*.md` constraint or a hook denial. For this
provider every byte at level 3 is unpatched upstream text, which changes nothing about where it sits.

**On workflow the order is inverted**: an invoked audited skill owns its documented engineering methodology —
agents, lanes, reviewers, writers, repair loops — and the project does not override it. See `GOVERNANCE_V3.md` §10, and the four preserved orchestration methodologies listed above.

The non-delegable prohibitions bind every agent at every delegation depth, whatever a skill's own text says:
no marking a PR ready for review, no merge or auto-merge (**the owner performs merges**), no
release/tag/deploy, no triggering or rerunning a GitHub Actions workflow, no GitHub settings or secrets
changes, no force push, no direct push to `main`/`master`. `plow-ahead` is the skill most likely to be read as
a blanket waiver of that list — it is not, and its own Stop Conditions point the same way. An instruction to
"keep going until done" ends where the prohibitions begin, and `agent-watchdog` watching a red CI run may
diagnose it but may never rerun it.

## Supply-chain assumptions

Same rationale as the other five providers: a live install trusts upstream's default branch on every future
run, not just the commit reviewed today. Vendoring converts that into "trust as of a specific reviewed SHA,
re-established only when someone deliberately re-reviews." That framing is sharper than usual here, because
the provider's own distribution channel is a floating one: every upstream `README.md` ends with
`npx @agent-native/skills@latest add --skill <name>`.

This provider's manifest uses the **file-level** schema: every `files[]` entry carries both an
`upstream_blob_sha` (what upstream had at the pin) and a `vendored_blob_sha` (`git hash-object` of the file as
committed here). `provider-file-hashes` fails closed on any on-disk edit to any vendored file — patched,
unpatched, or local-origin — that isn't accompanied by a deliberate `vendored_blob_sha` bump in the manifest.
That is the re-audit forcing function. With `GOVERNANCE_UPSTREAM_DIR_BUILDERIO` pointing at a read-only clone
at the pin, `upstream_blob_sha` is additionally verified against upstream itself, so divergence *from
upstream* is detected rather than mere self-consistency.

The installed slice is unusually low-risk on content: 15 files, zero executables, zero network calls of its
own, zero MCP dependencies. The residual surface is what the *instructions* would have an agent reach for, and
it was reviewed on that basis:

- **The five installed skills name no CLI, no MCP server and no hosted service.** `read-the-damn-docs` wants
  web search and degrades gracefully without it; that is the whole external surface.
- **The 21 `npx … @latest` invocations found in this provider are all in `HOLD`/`EXCLUDE` skills** — the
  `@agent-native/core` family in the three `visual-*` skills, `ccusage@latest` in `stay-within-limits`,
  `@agent-native/skills@latest` in installers. Three use `-y`/`--yes` to auto-accept. This is the single
  largest risk in the provider and the main reason three otherwise-strong skills are held rather than
  installed. None of it is in the installed set except the README install lines noted under
  "Known limitations".
- **The vendored `README.md` files are catalogue text, not agent instructions.** Claude Code loads `SKILL.md`;
  no `SKILL.md` in this set references its `README.md`. They were vendored anyway, verbatim, rather than
  omitted — see "Known limitations" for the trade-off that decision carries.

What was *not* done, stated plainly: no test suite, static-analysis scan, or dynamic execution was run against
this provider — there is no executable content in the installed set to run or scan. The review consisted of
reading all 13 candidate skills with their dependency closures, reading the two provider scripts end to end,
diffing the vendored tree against the pinned clone, and recording hashes.

## Second repository audited: `BuilderIO/builder-agent-skills`

A second Builder.io repository, `BuilderIO/builder-agent-skills`, was audited in the same round at pin
`3366b3527706cad45434f97bcd1feda6359af6eb` (authored 2026-08-06, *"feat: add allow-commands and grill-me
skills with usage instructions and documentation"*, depth-1 shallow clone). **Nothing was vendored from it.**
There is no `.claude/skills/` directory owned by it, no manifest, and no ledger. This section exists so the
audit is not repeated blind, and so the reason is on the record.

### The licence finding, verified directly

At that pin the repository has **no root `LICENSE`**. An exhaustive filename search across the whole tree for
`*license*` / `*copying*` / `*notice*` / `*copyright*` / `*patents*` returns exactly one file:
`hallmark/LICENSE`. There are no `package.json` files anywhere in the tree, so there is no `license` field to
inspect; `README.md` (381 lines) contains no licence section, statement or copyright line; and no skill
declares a `license` frontmatter key (the complete frontmatter key set across all 15 `SKILL.md` files plus the
two orchestrator agent files is `name`, `description`, `tools`, `model`, `disable-model-invocation`,
`version`, `allowed-tools`). This was re-verified while writing this document: `git ls-tree -r HEAD` over the
whole tree returns `hallmark/LICENSE` as the only licence file.

`hallmark/LICENSE` is verbatim, unmodified MIT text — `Copyright (c) 2026 Hallmark contributors`. Note the
holder: **Hallmark contributors, not Builder.io**. That is consistent with `hallmark` being an independently
developed upstream vendored *into* `builder-agent-skills` with its own licence carried along —
`hallmark/SKILL.md` links to `../site/css/tokens.css`, `../docs/recipes.md` and `../docs/study-examples.md`,
none of which exist in this repository (they are siblings in hallmark's own repo), and
`references/imagery-kit.md` points at `https://www.usehallmark.com`.

**Does the MIT grant extend to the `hallmark` directory?** Plausibly, yes: by ordinary convention a `LICENSE`
at a directory root covers that directory's contents, which would put all 99 files under `hallmark/`
(`LICENSE`, `SKILL.md` and 97 reference files — counted directly) under MIT and nothing outside it. MIT is one-way compatible into this
GPL-3.0 repository, so redistribution would be clean provided `hallmark/LICENSE` were copied alongside the
content. The honest caveats are that "Hallmark contributors" is an unidentified collective holder, that the
shallow clone provides no commit provenance to corroborate it, and that "plausibly, by directory convention"
is a weaker basis than the explicit repository-level grant every other vendored provider here carries. The
audit lane also checked for third-party content *inside* `hallmark` that might carry other terms and found
none: no code, no binaries, no fonts, no images; `typography.md` names font families as recommendations only
and warns "Never name a paid font in code without confirming the user is licensed"; `imagery-kit.md`
references vendor-hosted assets by absolute URL and documents a hand-built-SVG fallback.

### What `hallmark` would have been worth for Dashboard Stage 1

Genuinely a lot, which is why the audit is recorded rather than dismissed. The lane's assessment: it is
materially different in *role* from the installed `frontend-design`, which gives aesthetic direction.
`hallmark` gives (a) a structural-variety catalogue — 21 named macrostructures, 46 component archetypes, 4
genres, none of which `frontend-design` has; (b) a mechanical 69-gate post-emit QA rubric
(`references/slop-test.md`) with computable gates, including contrast gates 46-50 (APCA Lc / WCAG pairs, with
a "black-text-on-black-button" canary at gate 48), input-state gates 41-45, mobile gates 61-68 and
token-discipline gate 58; (c) three explicit verbs, of which `audit` is report-only ("Do not edit. Do not
redesign.") and `redesign` is non-destructive by contract; and (d) a token export path that lands directly on
this repo — `references/export-formats.md` maps its tokens to a Tailwind v4 `@theme` block, which is exactly
the shape of `internal/web/static/css/input.css`, and `references/genres/modern-minimal.md` names "dashboard"
as an explicit trigger. Everything is plain HTML plus CSS custom properties, so it applies to Go
`html/template` without adaptation, and it deliberately owns no chart guidance, so it would not collide with
`dataviz`. Zero shell scripts, zero exec bits, zero git/PR/merge/deploy content across all 99 files.

### The verdict actually reached

The audit lane's own candidate verdict for `hallmark` was **`INSTALL`**. The outcome for this repository is
that **nothing from `BuilderIO/builder-agent-skills` was vendored** — `hallmark` included. Those are two
different statements and this section records both rather than retconning the lane: a recommendation to
install is not an installation, and the state on disk is authoritative. The gap between them is the licence
basis described above — a directory-convention inference from a file whose copyright holder is an
unidentified collective, in a repository with no root grant — which is a weaker provenance footing than the
five installed providers stand on. Adopting `hallmark` later is a normal re-vendor: it needs its own manifest,
policy, ledger and dedicated Draft PR, and the licence question answered on a firmer basis than convention
(an upstream root `LICENSE`, or the `hallmark` upstream identified and pinned directly).

Two integration expectations the lane recorded, should that ever happen: `hallmark` writes `tokens.css` and
`.hallmark/{log.json,preflight.json}` at the **project root** by default (`SKILL.md` steps 0 and 6), which in
this repo belong under `internal/web/static/css/` and an ignored scratch path — a task-contract scoping line
handles it, no patch required; and its frontmatter carries `version: 1.0.0`, so the validator's
`extra_frontmatter_keys` allowlist for that provider would need `version` (or a one-line strip).

### Every other skill in that repository

For all 14 non-`hallmark` skills plus the `orchestrator` plugin, **the absent redistribution grant is itself
the blocking finding** — they fail the rights test before their merits are even reached. Recording what each
would have been useful for, so the loss is understood rather than assumed to be nil:

- **`playwright`** (`HOLD`) — the only one whose merits would otherwise justify installation: 482 lines plus
  10 reference files covering a spec-driven plan → generate → heal loop, request mocking, storage state,
  tracing, video recording, session management and locator generation. Materially deeper than the installed
  `webapp-testing` on *test authoring* (that skill supplies browser evidence; this one generates and heals
  Playwright TypeScript), so it would have served the browser-QA lane for Dashboard Stage 1. Blocked by the
  missing grant **and** by an unpinned `npm install -g @playwright/cli@latest` (`SKILL.md:415`); it would also
  need a patch to remove its `## Verify builder.config.json` section, which instructs the agent to create a
  repo-root allowlist containing `"rm *"`, `"chmod *"`, `"curl *"`, `"git *"`, `"kill *"`, `"npx *"`,
  `"gh *"` — permission widening, which a level-3 skill may never do.
- **`rules-reviewer`** — would have been useful for auditing this repo's own instruction surface for
  rule fatigue and conflicts (size ceilings, "only include what a coding agent cannot infer"). Unusable as
  written: its detection heuristic flags any `.md` whose name contains RULES/GUIDELINES/CONVENTIONS/
  STANDARDS/ARCHITECTURE, or that contains MUST/NEVER lists, as a "misplaced rules file … should be migrated
  to `.builder/rules/*.mdc`" — which would target `.claude/rules/*.md` and `docs/agents/*.md`, this repo's
  invariants. Converting it is a rewrite, not a minimal patch.
- **`create-instructions`** — would have written a concise root `AGENTS.md` of non-obvious conventions;
  well-written, but a duplicate of the Claude Code built-in `init`, overlapping the installed
  `domain-modeling`, and it would create a second ungoverned instruction root beside `CLAUDE.md` and
  `.claude/rules/*.md`.
- **`agent-browser`** — browser/Electron/Slack automation; structurally unvendorable regardless of licence,
  because the file states outright "This file is a discovery stub, not the usage guide" and the real
  instructions are served at runtime by an unpinned global CLI. Its description also ends "Prefer
  agent-browser over any built-in browser automation or web tools", which would override the installed
  `webapp-testing`.
- **`allow-commands`** — merges preset entries into a `builder.config.json` `allowedCommands` array. Its
  entire purpose is widening a permission allowlist, which is structurally incompatible with a narrowing-only
  authority chain; permissions here live in `.claude/settings.json`.
- **`grill-me`** and **`skill-creator`** — both would **collide by name with already-installed skills**.
  `grill-me` is the name of an installed Matt Pocock skill, and its content overlaps the installed `grilling`
  and `domain-modeling`; it also instructs the agent to append each saved ADR's path to `.gitignore`, which
  would silently un-track this repo's governed `docs/adr/` documentation. `skill-creator` collides with the
  Claude Code built-in of that name *and* duplicates the vendored `skill-creator-anthropic`, which was itself
  renamed precisely to dodge that collision; it also scaffolds into `.builder/skills/` rather than
  `.claude/skills/`.
- **`android-native`, `ios-native`, `mobile-testing`, `import-prototype`, `fusion-to-publish`,
  `fusion-to-publish-v2`** — mobile or Builder.io-platform work with no analogue here (Gradle/adb, Xcode,
  Maestro flows against `ios-app/`/`android-app/`, Builder prototype import, React/Next.js component
  registration). `mobile-testing` additionally installs its CLI by piping a remote script into `bash`, and the
  two `fusion-to-publish-v2` helper scripts are the only executable-bit files in that entire repository.
- **`unzip`** — a Builder.io-sandbox shell helper whose notes exist purely to route around that sandbox's ACL
  ("tar is blocked", "base64 -d is blocked"); it ends by calling a `DevServerRestart` tool that does not exist
  here.
- **`orchestrator`** — not a skill at all: no `SKILL.md`, just `agents/orchestrator.md` and `agents/worker.md`
  installed as a plugin. Governance v3 already makes skill-native orchestration the default, so a generic
  orchestrator/worker pair adds nothing.

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
`docs/agents/skills-update-automation.md` on the repository's default branch (`main`) — under GitHub
scheduled-workflow semantics the automation is defined and runs only there; this stable line is not
maintained by it and receives audited refreshes via the normal task-branch/PR path
(`docs/agents/skills-routing.md`, "Keeping the stack itself current").

## Update procedure

1. Fetch the new upstream commit into a read-only clone (never edit it in place); set
   `GOVERNANCE_UPSTREAM_DIR_BUILDERIO` to that clone's path for the validator's stricter blob-hash mode.
   Prefer a full clone over `--depth 1` so the next reviewer has provenance this one did not.
2. Diff the set of directories under `skills/` **and `.agents/skills/`** against the last-reviewed list —
   additions, removals and renames among the five installed skills, and whether any of the eight recorded
   candidates changed in a way that alters its verdict. The three `HOLD` entries are the ones to check first:
   a local-files mode that genuinely drops the hosted connector, or a pinned `@agent-native/core`, would
   change the answer.
3. Re-check the generated-export set. `scripts/sync-agent-native-skills.mjs` means `visual-plan`,
   `visual-recap`, `visual-edit` and `rewind` can change wholesale between pins without a reviewable
   commit-by-commit history in this repository. Read them fresh rather than diffing them.
4. For each vendored skill, diff every file in its `files[]` list against the **currently-vendored copy**. For
   this provider that is the same as diffing against upstream today (nothing is patched), but do not assume
   that stays true.
5. Re-run the same review judgment on anything new: does it assume background execution, external network
   fetch, credentials, an unconfigured MCP server, an unpinned CLI, or a write into a git working tree this
   project doesn't grant? If so, patch it minimally and mark it — the test is **authority**, not
   orchestration. A skill's agent topology, fan-out width, writer count, reviewer lanes and repair loops are
   never grounds for a patch (see "Default: minimal patching"). Re-check that no new `agents/openai.yaml` or
   other forbidden filename entered the installed set.
6. Update `upstream_commit`, `upstream_tree`, `upstream_current_head`, `drift` and `reviewed_at` in
   `builderio-skills-manifest.json`; update every touched file's `upstream_blob_sha`/`upstream_mode` and
   `locally_modified`/`patch_ids`. Recompute `vendored_blob_sha` (`git hash-object <path>`) for every touched
   file **last**, after all edits for the round are finalized — that is what re-pins the file and clears the
   fail-closed check.
7. Update `builderio-skills-patches.md` for any patch added, changed or removed (including a new
   `bio-openai-yaml-excluded` row if a newly installed skill ships that sidecar), and this policy's
   "Installed" / "Excluded / Held" sections if the set changed.
8. If the round also revisits `BuilderIO/builder-agent-skills`, check first whether a root `LICENSE` has
   appeared. That single fact is what gates every skill in that repository, `hallmark` included; without it
   the answer is unchanged and no further reading is needed.
9. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
10. Open the change as its own dedicated Draft PR (see `mattpocock-skills-policy.md`'s "Dedicated Draft PR
    requirement" — the same rule applies here); never bundle a skills re-vendor into an unrelated change. Get
    human review before merge; this policy forbids the agent from merging it.

## Rollback

1. Identify the last-known-good `upstream_commit` from `builderio-skills-manifest.json`'s git history.
2. Restore the five affected directories from that commit: `git checkout <sha> --
   .claude/skills/agent-watchdog .claude/skills/efficient-frontier .claude/skills/plan-arbiter
   .claude/skills/plow-ahead .claude/skills/read-the-damn-docs`.
3. Restore `docs/agents/builderio-skills-manifest.json` and `builderio-skills-patches.md` from the same commit
   (and this policy, if it moved with them).
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description.

Rolling back this provider is independent of the other five: no other manifest claims any of those five
directories, and no cross-provider patch id exists.

## Known limitations

- **Every vendored `README.md` ends with a floating install command** —
  `npx @agent-native/skills@latest add --skill <name>` (`agent-watchdog/README.md:42`,
  `efficient-frontier/README.md:49`, `plan-arbiter/README.md:40`, `plow-ahead/README.md:41`,
  `read-the-damn-docs/README.md:53`; `efficient-frontier`'s variant appends `--update-instructions`).
  Inside a pinned, vendored copy that line is misleading: following it
  would re-install the skill from a floating source, which is exactly what this policy exists to prevent. The
  audit lane recommended not vendoring the READMEs at all for this reason. They were vendored anyway, verbatim
  and unpatched, on the reasoning that Claude Code loads `SKILL.md` only, that no `SKILL.md` references its
  README, and that byte-identity to the pin is worth more than editing catalogue text an agent never reads as
  instruction. The trade-off is recorded here rather than hidden: if a future reviewer disagrees, the fix is
  to drop the five `README.md` files at the next re-vendor, not to patch them.
- **The installed descriptions are Codex-branded, unpatched.** `read-the-damn-docs`'s description ends "Forces
  Codex to web-search for current official docs before assuming from memory", and `agent-watchdog`'s opens
  with "from a Codex session ID". This provider publishes to a Codex marketplace as well as to Claude Code.
  The wording is cosmetic — it does not restrict the trigger surface in practice, and rewriting upstream prose
  for style is exactly what "no translations, no stylistic rewrites" forbids — but a reader may reasonably
  wonder why a Claude Code skill names Codex, and the answer is that it was left alone deliberately.
- **`plow-ahead` will be read as broad permission, and is not.** It is the one installed skill whose whole
  premise is "do not stop to ask". Its own Stop Conditions, the four-level authority chain, and the
  non-delegable prohibitions all still bind, and the operation mode still caps what it may write. An
  autonomy skill cannot widen an authority envelope; it only changes how many questions get asked inside one.
- **`agent-watchdog` polls.** "If the artifact is still running and the user asked to watch, poll at a
  reasonable interval until it is done, blocked, stale, or clearly waiting on a human/external system"
  (`SKILL.md:34-36`). That is a long-running read loop against GitHub or a local transcript, and it is
  bounded only by the skill's own judgment. It may read CI state; it may never trigger or rerun a workflow.
- **`efficient-frontier` assumes a cheaper model tier exists.** If the host exposes only one model, the cost
  premise does not hold — the delegation methodology still does, but the savings claim it inherits from
  `efficient-fable` does not apply. That claim is also, in upstream's own words, launch copy; treat "3-5x more
  cost-efficient" as marketing, not a measurement made here.
- **`read-the-damn-docs` presumes web search.** Without it the skill degrades to "say you could not verify"
  rather than failing loudly. That is the desired behaviour, but it means an answer produced under this skill
  in an offline session carries no more authority than one produced without it.
- **The three `HOLD` skills stay uninstalled until a re-vendor.** Provisioning the Plan or Design connector is
  a decision for the owner, not something an agent may infer from a task looking design-shaped.
- **The hook/permission layer is a backstop, not a substitute for reading a skill.** As
  `mattpocock-skills-policy.md`'s "Known limitations" section notes, a sufficiently subtle instruction can
  still shape agent *reasoning* even where it cannot force a blocked tool call. That caveat applies to this
  set identically and is not repeated in detail here.

## Compatibility

Reviewed and vendored against the same Claude Code version and governance-layer assumptions recorded in
`mattpocock-skills-policy.md`'s "Compatibility" section (frontmatter, permission and hook-payload behavior) —
see that document rather than duplicating the version pin here; re-verify them together if either changes.

Two provider-specific notes. First, Builder.io ships these skills to multiple agent runtimes at once (Claude
Code, Codex, its own Agent Native tooling), so a future pin may introduce host-specific frontmatter keys or
sidecar files beyond `agents/openai.yaml`; this provider's `extra_frontmatter_keys` allowlist is empty and
`no-forbidden-vendor-files` is fail-closed, which means such an addition surfaces as a validator failure
rather than being silently accepted — the intended behavior, and widening either is a reviewed decision, not a
fix. Second, part of this upstream is a downstream export of a private repository (see "Upstream"), so
"unchanged since the last pin" is only checkable for the hand-authored skills; for the generated four it means
"the export produced the same bytes", which is a weaker guarantee. All five installed skills are
hand-authored.
