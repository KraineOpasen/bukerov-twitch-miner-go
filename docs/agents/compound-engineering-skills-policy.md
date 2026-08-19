# Compound Engineering skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of
[EveryInc/compound-engineering-plugin](https://github.com/EveryInc/compound-engineering-plugin) into
`.claude/skills/` instead of installing it as a live Claude Code plugin. This document is the policy for what's
installed, why, how it's patched, and how to update it. See also
`docs/agents/compound-engineering-skills-manifest.json` (machine-readable inventory, file-level) and
`docs/agents/compound-engineering-skills-patches.md` (per-patch ledger). This is a separate vendored set from
the Matt Pocock, Anthropic, Trail of Bits, awesome-copilot and Builder.io sets; each upstream, manifest and
ledger is independent, and no skill name is shared across them (see
`scripts/validate-agent-governance.py`'s `manifest-ownership-partition` check).

## Upstream

- Repo: `https://github.com/EveryInc/compound-engineering-plugin`
- Reviewed commit: `67cc7dc7a11ab3724ca8e0723fcf18ee08e605de` (`docs(skill-design): third-sweep learnings`,
  #1483; advanced by fast-forward from `d6ae46457b3364ca1a3d6eb9954613217000c0ec`, 56 commits back)
- Plugin version at that commit: `3.22.4` (`plugin.json`), MIT, `"author": {"name": "Kieran Klaassen and
  Trevin Chow"}`
- Reviewed tree SHAs, one per vendored skill directory: recorded in the manifest's `upstream_tree` map (22
  entries).
- Current upstream HEAD at review time: **level with the pin** — `refs/heads/main` resolved to exactly
  `67cc7dc7a11ab3724ca8e0723fcf18ee08e605de` when this audit fetched it. There are no unaudited bytes ahead
  of the pin; see the manifest's `drift` field for the observation instant.
- Scope of the read: this pin was reached by a **deliberate re-vendor**, not a provenance refresh. The scheduled
  bot refused to prepare the candidate (issue #189: inventory changes in 19 skills, five frontmatter/authority
  surface changes, and four three-way merge conflicts), so the whole delta was re-derived independently rather
  than taken from that report. Upstream restructured by size: `SKILL.md` bodies were split into
  `references/*.md` (`ce-babysit-pr`'s went 90128 → 7939 bytes), giving **70 added and 70 modified files across
  20 of the 22 installed skills, 0 removed**; `ce-commit` and `ce-worktree` are byte-identical. Every one of
  those 140 files was read end to end against both pins, per skill, and all 20 changed skills were swept for
  new authority — merge, ready-flip, workflow rerun, direct-main push, force push, governance-file write,
  permission bypass, child-agent authority expansion. **Zero new patches were required: upstream added no
  authority in this update.** What it did do is *relocate* patched authority text, which is why the patch file
  lists moved so much (see "Local patches summary"). All three bundled scripts whose bytes changed
  (`ce-babysit-pr/scripts/pr-snapshot`, `ce-doc-review/scripts/cross-model-doc-review.sh`,
  `ce-resolve-pr-feedback/scripts/get-pr-comments`) were re-read end to end and behaviourally compared under
  `EVAL_REQUIRED`; see each skill's `audit_ref`. Nothing fetched from upstream was executed. Every
  `excluded_skills[]` entry was re-checked against its actual new bytes per the update procedure's step 2:
  **zero verdict changes**, including `ce-skill-work`, the one candidate the final commit touched. 22
  directories are installed; all 12 remaining candidates are recorded in the manifest's `excluded_skills[]`
  with a verdict and a reason (see "Excluded / Held").

## Installation model

**Project-local vendored copy**, not a live plugin install. Each installed skill's complete directory tree is
copied verbatim into `.claude/skills/<name>/` at review time, then minimally patched (see "Local patches
summary"), and every file's mode is normalized to `100644` — no executable bit anywhere under
`.claude/skills/**`. 345 files are tracked in total: 323 of upstream origin plus one per-skill `LICENSE` copy
in each of the 22 directories. (At the superseded pin this was 275 = 253 + 22; the restructure added 70
upstream files and removed none.)

`automatic_updates: false` — nothing about this installation re-fetches, re-syncs, or version-checks upstream
on its own. A human, or an agent task under an explicit contract, must re-run the review process to move the
pin (see "Update procedure").

### Self-contained by upstream design — no shared runtime tree

Upstream deliberately **duplicates** shared components into every skill that owns one instead of factoring
them out, and pins the duplicates with byte-parity tests. From `tests/skill-context-parity.test.ts`:

> `context.mjs` is byte-duplicated into every skill whose flows depend on subagent dispatch (the plugin has no
> cross-skill import mechanism — see AGENTS.md "File References in Skills"). All copies must stay identical.

The parity set, as it exists upstream at this pin:

| Shared asset | Upstream copies | Installed here | Parity test |
| --- | --- | --- | --- |
| `scripts/context.mjs` | 15 | 13 (the other two belong to `ce-retune` and `ce-sweep`, both excluded) | `tests/skill-context-parity.test.ts` |
| `scripts/peer-job-runner.py` | 6 | 6 | `tests/peer-job-runner-parity.test.ts` |
| `references/html-rendering.md`, `references/markdown-rendering.md` | 3 each | 3 each (`ce-brainstorm`, `ce-ideate`, `ce-plan`) | — |
| `references/reasoning-elevation.md` | 2 | 2 (`ce-plan`, `ce-brainstorm`) | `tests/reasoning-elevation-parity.test.ts` |
| `references/settled-decisions.md` | 2 | 2 (`ce-plan`, `ce-brainstorm`) | `tests/settled-decisions-parity.test.ts` |
| `scripts/elevation-dispatch.sh` | 2 | 2 (`ce-plan`, `ce-brainstorm`) | covered by `peer-job-runner-parity`'s `PEER_WORKERS` list |
| `scripts/light-webserver.js` | 2 | 2 (`ce-brainstorm`, `ce-prototype`) | — |
| `ce-compound` / `ce-compound-refresh` pairs: `scripts/validate-frontmatter.py`, `scripts/validate-doc-claims.py`, `references/schema.yaml`, `references/yaml-schema.md`, `references/concepts-vocabulary.md`, `assets/resolution-template.md` | 2 each | 2 each | — |

The practical consequence, verified at this pin: **no vendored skill references a sibling skill's files, and
no skill uses `${CLAUDE_PLUGIN_ROOT}` or `${CLAUDE_SKILL_DIR}`** — grep over the whole vendored tree returns
zero hits for either variable, and the only cross-skill `ce-*` path anywhere in the tree is a `/tmp` run-artifact
directory in `ce-code-review`'s own output contract. Each `.claude/skills/ce-*/` directory stands alone, and **there is no shared
runtime tree installed alongside them** — no plugin root, no sibling `scripts/` directory, nothing outside the
22 skill directories. `scripts/validate-agent-governance.py`'s `provider-dependency-closure` check enforces
the same property mechanically: an inline-code path such as `scripts/context.mjs` in any vendored `.md` must
resolve to a file that was actually vendored.

Every bundled script is located through an inline `SKILL_DIR` anchor the skill sets per Bash call, and invoked
through an explicit interpreter — `bash "$SKILL_DIR/scripts/check-health"`, `"$PY" "$SKILL_DIR/scripts/pr-snapshot"
snapshot …`, `"$NODE" "$SKILL_DIR/scripts/context.mjs"`. Nothing is executed straight off disk, which is why
normalizing 27 files from `100755` to `100644` breaks no invocation. The only bare `./scripts/...` strings in
the tree are Markdown link targets, not commands.

### What Compound Engineering writes at runtime

CE skills are artifact producers, and the owner should know where those artifacts land before running one.

- **Artifact root.** Every CE skill that writes an artifact resolves `<root>` through the same block
  (`ce-setup/SKILL.md`, the `<!-- ce-docs-root:start -->` section, duplicated into 21 files): read `docs_root`
  from `<repo-root>/.compound-engineering/config.yaml` **only**; unset → `<root>` is `docs`. A set value must
  be a repo-relative directory whose symlink-resolved path stays inside the repo and is neither the repo root
  nor under `.git/`; otherwise the skill must "stop with an error naming `docs_root` and the value — never
  fall back to `docs`." It **fails closed**, which is the desirable behaviour but also means a typo stops CE
  work rather than silently redirecting it.
- **Subdirectories in play:** `<root>/solutions/` (ce-compound, ce-compound-refresh), `<root>/plans/`
  (ce-plan, ce-work), `<root>/ideation/` (ce-ideate), `<root>/explainers/` (ce-explain).
- **Setting `docs_root` in a tracked `.compound-engineering/config.yaml` is the way to keep CE artifacts out of
  this repo's curated `docs/` tree** (`docs/agents/`, `docs/adr/`). No such file exists here today, so the
  default applies and CE would write into `docs/`.
- **Scratch** goes to `.context/compound-engineering/`; `ce-setup` offers (with consent) to add that path and
  `.compound-engineering/*.local.yaml` to `.gitignore`.
- **`ce-setup`'s own writes:** Phase 2 Step 5 always copies `references/config-template.yaml` to
  `<repo-root>/.compound-engineering/config.example.yaml`; Step 6 creates `config.yaml` from the same template
  only after the user approves, and never overwrites an existing `config.yaml` or `config.local.yaml`. It
  states plainly: "Do not create `config.local.yaml`." No vendored skill in this set writes
  `config.local.yaml` — `ce-work` and `ce-brainstorm` only *read* it when it exists.

**`.compound-engineering/` was deliberately not vendored.** Upstream's copy contains exactly one file,
`config.example.yaml`, and it is byte-identical to the vendored
`.claude/skills/ce-setup/references/config-template.yaml` (`git hash-object` on both:
`9353b48f232cd7e90eeec4472f8aae046af211a7`). Copying it would have added a second, drifting copy of a template
this repo already carries, in a directory whose real role is to hold the **consumer** repo's own optional
config — which does not exist here.

## Installed: 22 skills

All 22 are `VENDORED_VERBATIM` (upstream content, minimally patched — no rewrites, no translations). "Patches"
lists the patch ids touching that skill; `ce-mode-normalize` is mode-only.

| Skill | What it is | Patches |
| --- | --- | --- |
| `ce-strategy` | Create/update `STRATEGY.md`; upstream product grounding for the ideation lane. | — |
| `ce-brainstorm` | Turn a vague idea into a right-sized, requirements-only unified plan. | `ce-mode-normalize` |
| `ce-ideate` | Generate and evaluate grounded options before one is chosen. | — |
| `ce-pov` | Decisive graded verdict on an adoption question, a document, or an approach set; owns the named-peer cross-check. | `ce-mode-normalize`, `ce-no-permission-bypass` |
| `ce-prototype` | Throwaway prototype to settle a design question cheaply. | — |
| `ce-plan` | Structured implementation plan from requirements. | `ce-mode-normalize` |
| `ce-doc-review` | Role-lens review of an existing requirements/plan/spec document. | `ce-mode-normalize`, `ce-no-permission-bypass` |
| `ce-work` | Execute a plan or concrete build request end to end. | `ce-mode-normalize`, `ce-no-permission-bypass` |
| `ce-debug` | Diagnosis loop for bugs, regressions and failing CI. | `ce-no-direct-main-push` |
| `ce-simplify-code` | Behaviour-preserving simplification of settled, recently changed code. | — |
| `ce-optimize` | Metric-driven optimization loops with experiment worktrees. | `ce-mode-normalize` |
| `ce-worktree` | Set up isolated git worktrees for fresh or existing work. | — |
| `ce-code-review` | Multi-persona structured code review; report-only by default. | `ce-mode-normalize`, `ce-no-permission-bypass` |
| `ce-commit` | One commit with a value-communicating message. | — |
| `ce-commit-push-pr` | Commit, push, open a PR; also PR-description-only flows. | `ce-draft-pr-only`, `ce-no-merge-authority` |
| `ce-resolve-pr-feedback` | Resolve PR review comments and threads. | `ce-mode-normalize` |
| `ce-babysit-pr` | Continuous watch loop over an open PR: comments, CI, branch currency. | `ce-mode-normalize`, `ce-no-merge-authority`, `ce-no-workflow-rerun` |
| `ce-compound` | Capture a solved problem as a durable repo learning; maintain `CONCEPTS.md`. | `ce-mode-normalize`, `ce-no-governance-file-writes` |
| `ce-compound-refresh` | Audit the learnings store against the current codebase. | `ce-mode-normalize`, `ce-no-direct-main-push`, `ce-no-governance-file-writes` |
| `ce-explain` | Durable visual teaching artifact for a concept, diff or work window. | — |
| `ce-handoff` | Session handoff / continuity source resume. | — |
| `ce-setup` | CE health check and repo-local config helper. | `ce-mode-normalize` |

## Excluded / Held

Twelve candidates were read and not installed: ten **EXCLUDE**, two **HOLD**. Full reasons in the manifest's
`excluded_skills[]` (each entry's `reason` is capped at 900 characters there; the substance is restated here).

**HOLD — valuable, blocked on something concrete:**

- **`ce-dogfood`** — genuinely not a duplicate of the installed `webapp-testing`: it is diff-scoped (branch vs
  trunk), maps journeys as Mermaid flowcharts before deriving a test matrix, walks each flow as a named
  persona to find paper cuts, and finalizes a durable report. *What blocks it:* a hard external CLI. SKILL.md
  lines 23-29 require `agent-browser` and say "If not installed, stop … This workflow cannot function without
  it", while line 16 forbids the alternative this repo already has: "Do not use Chrome MCP tools
  (`mcp__claude-in-chrome__*`), any browser MCP integration, or other built-in browser-control tools."
  `agent-browser` is absent here (`command -v agent-browser` → not found), and `ce-setup`'s install string is
  unpinned (`CI=true npm install -g agent-browser … && agent-browser install`, the second command downloading
  a browser). *What would unblock it:* a pinned, reviewed `agent-browser` install the owner accepts, plus a Go
  dev-server recipe — its prerequisite list is `bin/dev` / `rails server` / `npm run dev`, none of which
  applies to `go run ./cmd/miner`.
- **`lfg`** — a strong autonomous plan → implement → simplify → review → fix → commit → push → PR → watch-CI
  pipeline with structured gates. Checked line by line and by grep across its whole tree: **no** merge, no
  auto-merge, no ready-for-review, no force push, no workflow trigger; step 10 states pipeline mode "stopped
  at 'CI decided,' not 'merged'". *What blocks it, stated against the set that actually shipped:* the
  manifest's reason was written against a smaller candidate slice and says "none are in this slice", which is
  now out of date — ten of its eleven skill dependencies (`ce-plan`, `ce-work`, `ce-simplify-code`,
  `ce-code-review`, `ce-commit-push-pr`, `ce-babysit-pr`, `ce-debug`, `ce-resolve-pr-feedback`, `ce-explain`,
  `ce-handoff`) are installed at this same pin. Two real blockers remain: (1) step 7 hard-invokes
  `ce-test-browser mode:pipeline`, and that skill is excluded (below), so the step dead-ends; (2) its step-8
  stack handoff still names `posture:stack-land`, a posture the vendored `ce-babysit-pr` no longer has —
  vendoring `lfg` would require its own `ce-no-merge-authority`-class patch to stay coherent. *What would
  unblock it:* replacing step 7's browser leg (or accepting a documented skip), and patching the
  `posture:stack-land` reference out of step 8.

**EXCLUDE — reviewed and not wanted:**

- **`ce-polish`** — its differentiating half is framework dev-server detection with no Go path:
  `scripts/detect-project-type.sh` matches only `{rails, next, vite, nuxt, astro, remix, sveltekit,
  procfile}`; `scripts/resolve-port.sh` probes configs this repo does not have and falls through to `3000`
  while the dashboard defaults elsewhere; `scripts/resolve-package-manager.sh` returns
  `__NO_PACKAGE_JSON__`. Eight of eleven references are per-framework recipes. What survives is a ten-line
  "user browses, says what's off, you fix it" loop already covered by the built-in `run` skill plus the
  installed `webapp-testing` and `frontend-design`.
- **`ce-product-pulse`** — requires `pulse_analytics_source` (posthog | mixpanel), `pulse_tracing_source`
  (sentry | datadog), `pulse_payments_source` (stripe) and named product events. This is a single static
  binary a user runs against their own Twitch account: no telemetry pipeline, no user base, no payments.
- **`ce-promote`** — launch and marketing copy. No lane covers promotional writing for a GPL-3.0 self-hosted
  miner, and its enhanced path binds to Spiral, Every's hosted writing product.
- **`ce-proof`** — a client for `proofeditor.ai`. It uploads local repository markdown to a third party:
  `POST https://www.proofeditor.ai/share/markdown` needs no authentication, the resulting doc is "ownerless
  until a signed-in Every user claims the doc", and emptying a doc "does **not** scrub comment marks". A real
  data-egress surface for a repo whose domain is OAuth device-code auth, persisted cookies and Discord
  webhooks.
- **`ce-retune`** — two independent disqualifiers. Its Phase 0 demands a run archive, a build selector and a
  repeatable corpus task, and says "If any is missing, **stop and say so**" — every invocation would terminate
  there. And its mutation target is wrong for this checkout: it cuts prose across `./skills`, which here is
  `.claude/skills/**`, vendored third-party bytes held at minimal patching under six provider policies.
- **`ce-riffrec-feedback-analysis`** — consumes `riffrec-*.zip` capture bundles this project does not produce;
  `scripts/analyze_riffrec_zip.py` shells out to `ffmpeg`/`ffprobe` (absent) and uploads media to
  `https://api.openai.com/v1/audio/transcriptions` with `OPENAI_API_KEY`.
- **`ce-skill-work`** (repo-local `.agents/skills/ce-skill-work`) — upstream's own maintainer skill for
  editing *their* `skills/**`. Broken closure when vendored: SKILL.md line 18 names
  `docs/solutions/skill-design/portable-agent-skill-authoring.md` as "the authority" and that file lives
  outside the skill directory; `references/new-skill.md` additionally requires `docs/skills/<name>.md`, a docs
  README, a root README and `tests/release-metadata.test.ts`. It also hardcodes `bun test` / `bun run
  release:validate` and cites upstream PR numbers as normative, and `references/evaluate.md` routes validation
  to a skill named `skill-creator`, which here is a Claude Code built-in and also collides with the vendored
  `skill-creator-anthropic`.
- **`ce-sweep`** — two of its three sources are Slack and email (the latter self-labelled "EXPERIMENTAL"),
  neither of which exists for this single-owner repo; the remaining github-issues source applies ack and
  close-out labels, a tracker mutation this project keeps read-only by default, on a low-traffic tracker that
  does not justify a lease-based state engine and media pipeline. **A second ground recorded in the manifest
  does not survive scrutiny — see "Where the audit lanes disagreed" below.**
- **`ce-test-browser`** — a true duplicate of the installed `webapp-testing`, and the two actively conflict:
  its SKILL.md line 22 says "Never install or substitute standalone Playwright, Puppeteer, a separately
  configured browser extension or MCP", which is exactly what `webapp-testing` is. Its route table is
  Rails/Next.js only, port detection reads `package.json` and `.env`, and `pipeline-orchestration.md` starts
  the server only via `bin/dev`, `bin/rails` or `package.json`, so pipeline mode fails deterministically here.
- **`ce-test-xcode`** — Xcode/iOS Simulator via XcodeBuildMCP. No Apple surface in a Linux Go project.

**Non-skill upstream paths** (`excluded_upstream_paths[]` in the manifest), worth naming because several are
more than scaffolding:

- **Plugin and marketplace manifests** — `plugin.json`, `.claude-plugin/`, `.agents/plugins/marketplace.json`.
  Installing as a plugin is the model this vendoring deliberately replaces.
- **The TypeScript converter CLI and its test suite** — `src/`, `tests/`, `package.json`, `bun.lock`,
  `tsconfig.json`. Upstream's tool for emitting this plugin into other agent formats; it is not a skill, and
  vendoring it would add a bun toolchain to a Go repo.
- **Ten non-Claude platform install directories** — `.agy/`, `.cline/`, `.codex-plugin/`, `.cursor-plugin/`,
  `.devin-plugin/`, `.grok-plugin/`, `.kimi-plugin/`, `.omp-plugin/`, `.opencode/`, `.pi/`. These are host
  integrations for other agent runtimes, and some mutate the host: `.cline/scripts/install-skills.sh` links
  skill directories into `${CLINE_SKILLS_DIR:-$HOME/.cline/skills}` with `ln -sfn`, and `.agy/INSTALL.md`
  documents `agy plugin install` staging a copy under the user's home directory. None of that belongs in a
  vendored, hash-pinned read-only tree.
- **Upstream's own release automation** — `.github/` (release-please, PR-title lint, `bun run
  release:validate` / `plugin:validate` / `bun test` as the merge gate) and `CHANGELOG.md`. This repo has its
  own CI and its own release story.
- **`AGENTS.md`** (52 KB; upstream's `CLAUDE.md` is a symlink to it) — copying it would import a competing
  operating contract into a repo whose `CLAUDE.md` is the authority. Its line 36 is a **merge policy** for
  *their* repo ("All changes to `main` go through pull requests. Direct pushes and direct merges are not
  allowed; branch protection on `main` enforces this by requiring the `test` status check to pass"), its
  line 177 names their CI as "the merge gate", and lines 93-95 forbid hand-editing their release-owned
  version and changelog files. All perfectly reasonable upstream; all wrong here.
- Remaining root files — `README.md`, `CONCEPTS.md`, `GEMINI.md`, `SECURITY.md`, `PRIVACY.md`, `assets/`,
  `docs/`, `scripts/`, `.claude/`, `.compound-engineering/` (see "Installation model").

### Where the audit lanes disagreed

One candidate produced a genuine split. The exclusion lane recorded, as an independent second ground for
excluding `ce-sweep`, that "`scripts/context.mjs` is engineered to override harness and system-prompt
constraints from tool output", citing its `AUTONOMY_DIRECTIVE_CHECK` and `SKILL.md:25`'s order to "follow the
directives it prints". The script lane, reading the same file for the installed set, preserved it verbatim in
13 skills.

**Deciding evidence: the two lanes were looking at the same bytes.** All 15 upstream copies of `context.mjs`
hash to `e0d7dae5b98d6feadb29c6f94ef37d65ff57d386`, and the Setup fence that says "follow the directives it
prints … do not pipe or filter it" is the same boilerplate in `ce-work`, `ce-code-review`, `ce-plan` and the
other installed dispatch skills. So that ground cannot distinguish `ce-sweep` from what was installed — it is
either a reason to exclude 14 skills or a reason to exclude none. `ce-sweep`'s exclusion stands on its first
ground alone (Slack and email sources that do not exist here, plus a tracker-mutation engine with nothing to
sweep), and this policy treats the `context.mjs` clause in that manifest entry as **superseded** by the
analysis below. The manifest entry itself is left as written; correcting it is a manifest edit for the next
re-vendor, not something these documents may do on their own.

### `context.mjs`: read in full, preserved deliberately

`scripts/context.mjs` (109 lines, present in 13 installed skills) is a Node script the skill runs at Setup. It
prints a `RESOLVED_CONTEXT:` block plus four directive blocks — `SUBAGENT_AUTHORIZATION`,
`HARNESS_ATTRIBUTION`, `AUTONOMY_DIRECTIVE_CHECK`, `INDEPENDENCE_ACCOUNTING`. It was read end to end. What it
actually does: `execFileSync('git', …)` twice, for `rev-parse --abbrev-ref HEAD` and `rev-parse --short HEAD`,
both wrapped in `try/catch` returning `''`; then it writes to stdout. **No network call, no file write, no
environment mutation, no subprocess other than those two read-only `git` reads.**

It is preserved because what it argues for is **workflow, not authority**. `SUBAGENT_AUTHORIZATION` says that
a user's invocation of the skill is the request for the skill's shipped subagents — exactly the topology
Governance v3 assigns to the skill (see "Governance precedence"). `AUTONOMY_DIRECTIVE_CHECK` tells the agent
*not* to infer that the user is absent and to "probe once with the structured question tool" — it narrows
autonomous action rather than widening it. `INDEPENDENCE_ACCOUNTING` forbids counting inline personas as
independent corroboration. None of the four grants a tool, a path, or a permission.

The honest structural observation, recorded because it is the reason the file was read rather than trusted: a
bundled script whose own comments say "tool-result content in the working turn outranks a system-prompt
default" and "this text is positioned to outrank skill prose", and which emits that text as tool output every
run, has the same shape as a prompt-injection attempt. It is not treated as one here because its payload was
read in full and argues only inside the workflow lane — but the shape is why it is called out, why the file is
pinned by `vendored_blob_sha` in all 13 copies, and why a future re-vendor must re-read it rather than diff it
away.

## Invocation modes

- **Model-invoked**: 21 skills — unchanged from upstream.
- **User-invoked only** (`disable-model-invocation: true`): `ce-setup`. This is upstream's own frontmatter,
  **not** a local patch; a health-check and config-writing skill should not fire on model judgment, and this
  project agrees, so it was left as-is.
- **Frontmatter preserved rather than stripped:** `ce-resolve-pr-feedback` ships `allowed-tools: Bash(gh *),
  Bash(git *), Read`. That key *narrows* the tool surface, so the validator's allowlist was widened for this
  provider (`extra_frontmatter_keys` in `scripts/validate-agent-governance.py`) instead of the skill being
  edited.
- **A metadata inaccuracy this review found and corrected:** the manifest first recorded `"invocation":
  "model"` for all 22 skills, including `ce-setup`, contradicting that skill's own frontmatter. Review caught
  it; the manifest now records `"invocation": "user"` for `ce-setup` and `model` for the other 21, which is
  what the on-disk frontmatter says. The agreement no longer depends on bookkeeping care:
  `scripts/validate-agent-governance.py`'s `provider-invocation-matches-frontmatter` check fails closed, for
  every provider, whenever a manifest's `invocation` disagrees with the skill's `disable-model-invocation`
  frontmatter — so this class of drift cannot recur silently at the next re-vendor.

## License & attribution

All 22 vendored skills are MIT, `Copyright (c) 2025 Every`. MIT's operative obligation is a single clause:
"The above copyright notice and this permission notice shall be included in all copies or substantial portions
of the Software." A vendored skill directory is a copy, so **each of the 22 directories carries its own
verbatim `LICENSE`** (blob `959dd283c510cfac6fd912555f99b14613ee9018`, copied from the upstream repository
root, recorded in the manifest with `origin: "local"` and a reason) rather than relying on one shared notice
elsewhere in the tree. The validator enforces this as `license: {"spdx": "MIT", "layout": "per-skill",
"filename": "LICENSE"}`.

MIT has no Apache-2.0 §4(b)-style "mark your modified files" requirement, so the marker-and-ledger discipline
used here is **this project's own convention**, not a licence obligation: every locally patched file carries a
`bukerov-local-patch` marker pointing at a row in `compound-engineering-skills-patches.md`. It is kept
identical across providers so one reviewer habit works everywhere.

MIT is one-way compatible into GPL-3.0: MIT-licensed code may be included in a GPLv3 work (the combined work
ships under GPLv3, with the MIT notice retained), and not the reverse. That is the direction this repository
needs.

## Local patches summary

Seven patch ids, touching 25 files by content and 27 files by mode. Full ledger, one row per patch id per
touched file (57 rows: 30 content + 27 mode): `docs/agents/compound-engineering-skills-patches.md`.

**These counts moved sharply at the `d6ae4645 → 67cc7dc7` re-vendor, and the reason matters.** Upstream's
size-driven restructure moved prose out of `SKILL.md` into `references/*.md`, and the patched text moved
with it — so patches had to follow their text rather than stay on their recorded paths. Content-patched
files went 15 → 25. No patch id was created and none was retired; all seven survive, re-placed. Two
`SKILL.md` files (`ce-compound-refresh`, `ce-work`) stopped being patch targets entirely, having become
pointer shells. Re-applying the previous file list would have left dead patches on those two while leaving
the relocated authority — including the full `stack-land` land step with `gh stack merge … --yes --squash`
— unpatched in the new files it moved to, and a blob-hash check alone would not have caught it.

(The superseded revision of this document recorded "14 files by content … 45 rows", which was already
wrong against the ledger and the tree at that pin: 15 files / 46 rows. That pre-existing off-by-one was
flagged as a deferred finding by the previous audit and is corrected here.)

| Patch id | Files | Ground |
| --- | --- | --- |
| `ce-no-merge-authority` | 13 (10 in `ce-babysit-pr`, 3 in `ce-commit-push-pr`) | (a) owner-only merge boundary |
| `ce-draft-pr-only` | 3 (`ce-commit-push-pr`) | (a) owner-only Ready-for-review boundary |
| `ce-no-workflow-rerun` | 3 (`ce-babysit-pr`: `SKILL.md`, `tick.md`, `envelope.md`) | (a) + (c) — reruns are owner-only *and* denied by `.claude/settings.json` |
| `ce-no-direct-main-push` | 2 (`ce-compound-refresh`, `ce-debug`) | (b) direct-main-push prevention |
| `ce-no-governance-file-writes` | 3 (2 in `ce-compound`, 1 in `ce-compound-refresh`) | (f) integrity — a skill must not rewrite the layer that authorizes it |
| `ce-no-permission-bypass` | 6 (4 scripts, one spec enum, one prose line in `ce-work/references/execution-strategy.md`) | (f) integrity + (c) incompatibility |
| `ce-mode-normalize` | 27 (mode only, content untouched) | (c) project convention: no exec bit under `.claude/skills/**` |

Mapping to the six legitimate grounds this project recognizes for patching a vendored skill:

- **(a) Owner-only Ready/merge boundary** — `ce-no-merge-authority` deletes upstream's `posture:stack-land`
  (the posture that makes "selecting or handing off `posture:stack-land` **is** run-level land authorization"
  and then runs `gh stack merge … --yes --squash` + `gh stack sync`) from `ce-babysit-pr` and every reference
  to it; `ce-draft-pr-only` makes PR creation `gh pr create --draft` and drops `gh stack submit`'s `--open`
  flag, which "creates PRs ready for review instead of drafts, and also marks pre-existing drafts ready". The
  loop still *prints* the exact `gh stack merge <N> --yes --squash` command when a PR looks ready — printing
  is the whole point. **The owner performs merges.**
- **(b) Direct-main / force-push prevention** — `ce-no-direct-main-push` removes "commit directly to the
  current branch" from `ce-compound-refresh`'s default-branch option set, and adds a stop-and-report guard to
  `ce-debug`'s pipeline mode, which otherwise commits and pushes whatever branch it finds itself on.
- **(c) Concrete project incompatibility** — `ce-no-workflow-rerun` (`.claude/settings.json` denies
  `Bash(gh run rerun *)`, so the upstream instruction could not execute here even if it were wanted) and
  `ce-mode-normalize`.
- **(d) Broken dependency closure** — **no patch on this ground.** The installed set's closure is complete;
  `provider-dependency-closure` passes without one. Broken closure is why `ce-skill-work` was *excluded*, not
  patched.
- **(e) Licence / provenance necessity** — **no patch on this ground.** MIT was satisfied by adding a
  `LICENSE` file per directory; no upstream byte had to change.
- **(f) A genuine authority/integrity issue** — `ce-no-governance-file-writes` and `ce-no-permission-bypass`.

### `ce-no-permission-bypass` in detail

Four cross-model scripts dispatched peer models with their permission layers switched off. What was dropped,
and what was kept:

| Script | Upstream | Vendored |
| --- | --- | --- |
| `ce-work/scripts/cross-model-work.sh` (claude route) | `--permission-mode bypassPermissions --tools Read,Write,Edit,Bash --allowed-tools 'Bash(*)'` | `--permission-mode acceptEdits --tools Read,Write,Edit,Bash` — blanket `Bash(*)` grant dropped |
| `ce-work/scripts/cross-model-work.sh` (codex route) | `codex exec --ignore-user-config --ignore-rules --ephemeral -s workspace-write …` | `codex exec --ephemeral -s workspace-write …` — the two flags that discard the operator's own config and rules are gone; the sandbox stays |
| `ce-work/scripts/cross-model-work.sh` (cursor/composer/grok-cursor routes) | `cursor-agent -p … --force --sandbox enabled --trust …` | `--force` and `--trust` dropped; `--sandbox enabled` stays |
| `ce-code-review/scripts/cross-model-adversarial-review.sh`, `ce-doc-review/scripts/cross-model-doc-review.sh`, `ce-pov/scripts/cross-model-pov.sh` | `cursor-agent -p --mode ask --trust --sandbox enabled …` | `--trust` dropped; `--mode ask --sandbox enabled` stay |

Every edit is subtractive and leaves the sandbox in place; no route was disabled and no adapter was rewritten.
Note also that this repo sets `"disableBypassPermissionsMode": "disable"` in `.claude/settings.json`, so the
`bypassPermissions` route could not have worked here anyway — the patch removes an instruction that would have
failed loudly, and removes it for the right reason rather than leaving it to fail.

Two further files carry this id. One is prose, and it is mechanical: the note warning not to pass
`mode: "auto"` because "it overrides user-level settings like `bypassPermissions`" — which upstream moved
out of `ce-work/SKILL.md` into `ce-work/references/execution-strategy.md` at this pin. The instruction is
unchanged; only the illustrative example becomes `acceptEdits`, because naming a mode this repo disables
outright as the setting worth preserving is misleading. The other is
`ce-optimize/references/optimize-spec-schema.yaml`, whose `codex_security` enum offered `yolo`
(`--dangerously-bypass-approvals-and-sandbox`) as one of exactly two selectable postures, with a live step
applying whichever is selected; the value is removed and `full-auto` retained. That step also moved in the
restructure, so the patch's own note was corrected to cite `references/loop.md` § 3.2 step 5 rather than
`SKILL.md` step 5 — a patch rationale can go stale by relocation just as a patch can.

### `ce-mode-normalize`

27 files whose upstream mode is `100755` are stored `100644`. Content is untouched — each file's
`upstream_blob_sha` still matches on disk, except for the four cross-model scripts, whose content change is
recorded separately under `ce-no-permission-bypass`. The set of 27 is unchanged at this pin; two blobs were
re-pinned because upstream changed them (`ce-babysit-pr/scripts/pr-snapshot`,
`ce-resolve-pr-feedback/scripts/get-pr-comments`), and both were re-audited end to end rather than merely
re-hashed — a changed blob under `scripts_audited: true` requires re-audit, not a hash bump. Vendored files are content an agent reads and then runs
through an explicit interpreter, never a binary invoked off disk (see "Self-contained by upstream design"), so
`.claude/skills/**` carries no executable bit at all (`no-symlinks-no-exec-under-claude`), and
`provider-vendored-modes` fails closed on any mode difference no patch id documents, in either direction.

### Default: minimal patching

Under Governance v3 (`docs/adr/0002-governance-v3-skill-native-orchestration.md`), skills are preserved as
close to their authors' intent as practical. **Do not patch a skill merely because it uses subagents, several
writers, reviewers/critics, parallel analysis, iterative fixes, or its own handoff/orchestration pattern** —
including fan-out upstream leaves unbounded. That is engineering workflow, and workflow belongs to the skill
(see `docs/agents/agent-orchestration.md`). Patch only for concrete project incompatibility, a broken
dependency, licence/provenance necessity, or a genuine authority/integrity boundary.

This set is the sharpest test of that rule in the repository, and it was applied literally. Compound
Engineering is a heavily orchestrated plugin: `ce-code-review` dispatches a persona catalogue of reviewers in
parallel and then validators over their findings; `ce-work` runs subagent units against a shared workspace;
`ce-doc-review` promotes a finding only when two independently dispatched personas agree; `ce-brainstorm` and
`ce-plan` elevate reasoning through peer-model dispatch; `ce-babysit-pr` runs a self-sustaining watch loop that
delegates to `ce-resolve-pr-feedback` and `ce-debug` and repairs in a loop. **None of that was touched.** The
`context.mjs` preamble that exists specifically to keep those subagents from being suppressed was preserved
too. Every one of the seven patch ids removes an *authority* — a merge, a ready-flip, a workflow rerun, a push
to `main`, a write into the governance layer, a permission bypass — or normalizes a file mode. Not one narrows
a lane, a writer count, a fan-out width, or a repair loop.

## Governance precedence

Vendored skills sit **below** this project's own policy **on authority**. The authority chain has exactly four
levels (see `CLAUDE.md`'s "Claude Code Governance (v3)" section and `docs/agents/agent-orchestration.md`),
narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched, this set alongside every other.
4. **Generic model behavior** — fallback only.

Unpatched upstream text is **not** a separate tier below the patches: a vendored skill's instructions are
whatever its vendored bytes say, patched and unpatched alike, and they all sit together at level 3. Where a
local patch and the upstream text around it disagree, the patch wins — that is what patching means, and it is
resolved inside level 3 rather than by a fifth level. A skill instruction never overrides a
`.claude/rules/*.md` constraint or a hook denial.

**On workflow the order is inverted**: an invoked audited skill owns its documented engineering methodology —
agents, lanes, reviewers, writers, repair loops — and the project does not override it. Invoking one of these
skills authorizes the agent topology that skill documents, with no separate prompt-level permission. See
`docs/agents/agent-orchestration.md`.

Non-delegable at every delegation depth, and unaffected by anything any skill in this set says: marking a PR
ready for review, merge or auto-merge, release/tag/deploy, triggering or rerunning a GitHub Actions workflow,
changing GitHub settings or secrets, force push, and any direct push to `main`/`master`. **The owner performs
merges.** These require a separate, explicit, direct user command outside this policy, and even then are not
executed autonomously.

## Supply-chain assumptions

Same rationale as the other vendored sets: a live plugin install trusts upstream's default branch on every
future run, not just the commit reviewed today. Vendoring converts that into "trust as of a specific reviewed
SHA, re-established only when someone deliberately re-reviews." That matters more here than for a prose-only
provider — this set ships 53 files under `scripts/` directories (`.py`, `.sh`, `.mjs`, `.js` and six
extensionless), several of which drive `gh`, `git`, and external model CLIs.

The manifest tracks hashes **per file**. Every `files[]` entry carries a `vendored_blob_sha` — the `git
hash-object` of the file as committed here right now. For an unmodified file it equals `upstream_blob_sha`
(both checked); for a patched file or the local-origin `LICENSE` copies it is the only hash pinning that
file's content. `scripts/validate-agent-governance.py`'s `provider-file-hashes` fails closed on **any** on-disk
edit to any vendored file that is not accompanied by a deliberate `vendored_blob_sha` bump — which is the
re-audit forcing function: `context.mjs`, `pr-snapshot`, `peer-job-runner.py` and the four cross-model scripts
cannot be quietly edited with the validator staying green. Set
`GOVERNANCE_UPSTREAM_DIR_COMPOUND_ENGINEERING` to a read-only clone at the pin to additionally verify every
`upstream_blob_sha` against upstream rather than only locally.

`scripts_audited: true` with an `audit_ref` records that every bundled script in that skill was read
end-to-end during this review, not merely diffed. It is set on 17 skills; the remaining five (`ce-commit`,
`ce-commit-push-pr`, `ce-handoff`, `ce-strategy`, `ce-worktree`) ship no `scripts/` directory at all and
carry `null`.

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
   `GOVERNANCE_UPSTREAM_DIR_COMPOUND_ENGINEERING` to that clone's path for the validator's stricter blob-hash
   mode.
2. Diff the set of directories under `skills/` against the last-reviewed list — additions, removals, renames
   among the 22 installed skills — and re-check every `excluded_skills[]` entry: a HOLD may have become
   installable (`agent-browser` pinned, `ce-test-browser` resolved), and an EXCLUDE may have acquired a new
   dependency that changes the verdict.
3. Re-run the parity check before trusting any single copy: upstream duplicates shared assets, so a change to
   `context.mjs` or `peer-job-runner.py` lands in every consumer at once. Diff one copy, then confirm the rest
   are byte-identical to it, exactly as upstream's own parity tests do.
4. For each vendored skill, diff every file in its `files[]` list against the **currently-vendored copy** (not
   raw upstream — 14 files are patched) to isolate genuinely new upstream content from an old patch.
5. Re-run the same review judgment on anything new. The test is **authority**, not orchestration: does it
   merge, mark ready, rerun a workflow, push to `main`, force-push, write into `.claude/**` or `CLAUDE.md`, or
   bypass the permission layer? Then patch it minimally and mark it. A skill's agent topology, fan-out width,
   writer count, reviewer lanes and repair loops are **not** grounds for a patch (see "Default: minimal
   patching"). Read every new or changed script end to end and re-confirm `scripts_audited`.
6. Update `upstream_commit`, `upstream_version`, `upstream_tree`, `upstream_current_head`, `drift`,
   `reviewed_at` and `reviewed_by`; update every touched file's `upstream_blob_sha`/`upstream_mode` and its
   `locally_modified`/`patch_ids`. Recompute `vendored_blob_sha` (`git hash-object <path>`) for every touched
   file **last**, after all edits for the round are final — that is what re-pins the file and clears the
   fail-closed check.
7. Update `compound-engineering-skills-patches.md` for any patch added, changed, or removed, and this policy
   for any count that moved.
8. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
9. Open the change as its own dedicated Draft PR — never bundle a skills re-vendor into an unrelated change
   (see `mattpocock-skills-policy.md`'s "Dedicated Draft PR requirement"; the same rule applies here). Get
   human review before merge; this policy forbids the agent from merging it.

## Rollback

1. Identify the last-known-good `upstream_commit` from `compound-engineering-skills-manifest.json`'s git
   history.
2. Restore all 22 directories from that commit: `git checkout <sha> -- .claude/skills/ce-babysit-pr
   .claude/skills/ce-brainstorm … .claude/skills/ce-worktree` (the manifest's `skills[].path` list is the
   authoritative set; restoring a subset can break the parity invariant, since a `context.mjs` or
   `peer-job-runner.py` change spans many directories at once).
3. Restore `docs/agents/compound-engineering-skills-manifest.json` and
   `compound-engineering-skills-patches.md` from the same commit.
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description.

## Known limitations

- **`gh` was not on `PATH` in the environment where this review ran** (`command -v gh` → not found). Four
  installed skills are built on it — `ce-babysit-pr`, `ce-resolve-pr-feedback`, `ce-commit-push-pr` and
  `ce-debug`'s CI paths — and the bundled scripts among them (`pr-snapshot` in the first; `get-pr-comments`,
  `get-thread-for-comment`, `reply-to-pr-thread`, `resolve-pr-thread` in the second) shell out to `gh`. They will
  fail plainly rather than silently degrade, and nothing in this project's tooling installs `gh`.
- **The cross-model peer routes need CLIs that are not installed** — `codex`, `cursor-agent` and `grok` were
  all absent at review time (`claude`, `node`, `python3` and `jq` were present). The cross-model scripts in
  `ce-work`, `ce-code-review`, `ce-doc-review` and `ce-pov` are therefore unexercised here; their patched
  adapter argv lines were verified by reading, not by running a peer dispatch.
- **`ce-draft-pr-only` has a real behavioural consequence.** `ce-commit-push-pr` now always creates draft PRs,
  and its own text says drafts are a "hard residual / reopen step before babysit handoff" because
  `ce-babysit-pr` skips drafts by default. So the documented auto-handoff will report a draft-only residual
  instead of starting a watch until the owner marks the PR ready. That is the intended trade: the owner owns
  the ready-flip.
- **`ce-no-workflow-rerun` leaves red checks red.** `ce-babysit-pr`'s flaky/infra branch now surfaces a rerun
  residual naming the host-qualified run ID instead of calling `gh run rerun`. The loop keeps working every
  other stream around it, but a genuinely flaky job stays failing until the owner reruns it.
- **`docs_root` defaults to `docs/`.** With no `.compound-engineering/config.yaml` in this repo, CE artifacts
  would land in the curated `docs/` tree next to `docs/agents/` and `docs/adr/`. Setting `docs_root` in a
  tracked `config.yaml` redirects them; an invalid value stops the skill rather than falling back.
- **`ce-setup` refreshes `.compound-engineering/config.example.yaml` unconditionally** once Phase 2 runs in a
  git repo — it is the one CE write that is not consent-gated. It writes a file byte-identical to the vendored
  template, so the effect is bounded, but it is a repo write.
- **`ce-setup/scripts/check-health` now depends on a usable `/dev/fd` on its always-taken path.** At this pin
  upstream replaced two here-string reads (`<<< "$x"`) with process substitution
  (`< <(printf '%s\n' "$x")`) to stop requiring a writable `$TMPDIR` in read-only sandboxes. The audit ran both
  versions against identical fixtures: on bash 5.2.21 with a usable `/dev/fd` they are byte-identical in
  stdout, stderr and exit status, with no filesystem write, no git mutation and no network syscall. Where
  `/dev/fd` is *unusable at runtime* — a bash built with `HAVE_DEV_FD` but run without `/proc`, for instance —
  bash offers no FIFO fallback, so both reads fail and the report degrades from `Optional capabilities 1/5`
  with correct rows to `0/5` with blank rows, plus one stderr line per iteration. The direction is fail-safe:
  it under-reports optional tools, never fabricates or executes an install command, never writes, never
  reaches the network, and still exits `0` — and the old bytes already carried the same `/dev/fd` dependency
  conditionally, at the `work_engine_preferences` read. The bytes were accepted verbatim rather than patched:
  reverting would put a content patch on a file that is mode-only today and reinstate exactly what upstream
  removed. Note for the next re-vendor: process substitution sets `$!`, inert here only because the script
  uses no `$!`, `wait`, `trap` or job control.
- **The `context.mjs` clause in the `ce-sweep` exclusion reason is superseded**, per "Where the audit lanes
  disagreed". The exclusion itself is not in question.
- **`ce-babysit-pr`'s watch loop needs a harness background-and-wake capability.** Without one it degrades to
  "checkpoint" — one tick, then a printed re-run command. It explicitly refuses to fake a loop with a
  foreground `sleep`, which Claude Code blocks anyway.
- **`node` is required for the Setup fence** in 13 skills; when absent the fence prints "no Node runtime;
  continue with the skill's normal behavior" and the skill proceeds unchanged. That is upstream's own
  degrade path, preserved.
- **This document's hook and permission-layer caveats are shared with `mattpocock-skills-policy.md`** — see
  that document's "Known limitations" for the parts of this project's enforcement model (residual Bash
  evasions, operation modes not mechanically enforced, MCP allowlist gaps) that apply equally here and are not
  repeated.

## Compatibility

Reviewed and vendored against the same Claude Code version and governance-layer assumptions recorded in
`mattpocock-skills-policy.md`'s "Compatibility" section (frontmatter handling, `Edit(path)` permission
semantics, PreToolUse hook payload shape) — see that document rather than duplicating the version pin here;
re-verify both together if either changes. On the upstream side the pin is plugin `3.22.1` at
`d6ae46457b3364ca1a3d6eb9954613217000c0ec`; Compound Engineering releases frequently, so expect real content
drift on any re-vendor rather than a provenance-only pin bump. That prediction held on the very next advance:
this pin's audit found upstream already two commits further on, touching three installed skills.
