# GitHub awesome-copilot skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of
[github/awesome-copilot](https://github.com/github/awesome-copilot) into `.claude/skills/` instead of
installing it as a live Claude Code plugin or Copilot extension. This document is the policy for what's
installed, why, how it's patched, and how to update it. See also
`docs/agents/awesome-copilot-skills-manifest.json` (machine-readable inventory, file-level) and
`docs/agents/awesome-copilot-skills-patches.md` (per-patch ledger).

This is one of six independent vendored sets (`mattpocock`, `anthropic`, `compound-engineering`,
`trailofbits`, `awesome-copilot`, `builderio`); each has its own upstream, manifest and ledger, and no two
may claim the same directory under `.claude/skills/` — enforced by
`scripts/validate-agent-governance.py`'s `manifest-ownership-partition` check. That partition is not
theoretical for this provider: upstream ships a `skills/codeql` directory and so does trailofbits, and the
Trail of Bits one is what is installed (see "Excluded / Held" below).

## Upstream

- Repo: `https://github.com/github/awesome-copilot`
- Reviewed commit: `a80885b76044550770f60f360f8a0e5ae3524a31` (authored 2026-08-14)
- Reviewed tree SHAs (per vendored skill directory): `github-actions-efficiency`
  `a9041cbe0a768c8c11c26c2524e3a780035988f5`, `github-actions-hardening`
  `25d75a622f5f8c034be5b25bf6585850d6f7fcd7`, `harness-engineering`
  `7135eb67252e4d77c70f388b6cdcbaaa684d3dc7`, `threat-model-analyst`
  `4c51abac7859b9529f721ae5043fffc2f9812b5a`, `web-design-reviewer`
  `eded2bc5a7d38261501a76b2e4913b703cc547ec`
- Current upstream HEAD at review time: same SHA (**drift: none**)
- Corpus size at the pin: **408 directories under `skills/`**, of which **5** are installed here.

One property of this upstream shaped the whole review: `github/awesome-copilot` is a Copilot-first
catalogue, not a Claude Code skill library. Several entries are prompt files or Copilot custom agents
wearing skill frontmatter — `architecture-blueprint-generator`'s body is a `## Generated Prompt` block full
of unevaluated `${VAR}` template ternaries, and `ai-team-orchestration`'s three `@ai-team-*` participants
live outside its skill directory in the clone-root `agents/*.agent.md` format. Under Claude Code an agent
reads that raw. Format-fitness therefore had to be checked per candidate rather than assumed from the fact
that a directory sits under `skills/`.

## Installation model

**Project-local vendored copy**, not a live install. Each skill's files are copied verbatim into
`.claude/skills/<name>/` at review time, then minimally patched (see below), and every file's mode is
normalized to `100644` — no executable bits anywhere under `.claude/skills/**`
(`no-symlinks-no-exec-under-claude`). `automatic_updates: false` — nothing about this installation
re-fetches or re-syncs from upstream on its own. A human, or an explicitly-contracted agent task, must
re-run the review process to move the pin.

The mode-normalization rule binds this provider like every other, but it produced **no patch rows here**:
all 32 upstream-origin files in the installed set are already `100644` at the pin. Upstream does ship
executables — 28 blobs at `100755` at this commit, 9 of them under `skills/` (including
`skills/quality-playbook/quality_gate.py`, which the sweep read and rejected) — and other providers here do
carry a `*-mode-normalize` id for exactly that case. None of those 9 is in this selection. The rule is still checked mechanically in both directions —
`provider-vendored-modes` fails closed on any `upstream_mode`/`vendored_mode` difference that no patch id
documents — so a future re-vendor that pulls in an executable file cannot record the normalization
silently. See the ledger's note on the reserved `ghac-mode-normalize` id.

The vendored set is 37 files: 32 upstream-origin plus 5 local-origin `LICENSE` copies (one per skill dir,
see "License & attribution"). **Every one of the 32 is Markdown.** This provider's installed slice ships no
Python, no shell, no HTML, no JSON — which is why `scripts_audited` is `null` for all five skills rather
than `true`: there was no executable content to audit, not an audit that was skipped.

## Selection: how the 408-skill corpus was swept

**Coverage here is a sweep, not an exhaustive read.** The 408 skill directories were not read end to end,
and this document does not claim they were. The method was:

1. **Sweep by signal.** Every directory name and every `SKILL.md` frontmatter `description` was scanned
   against this project's actual concerns: Go, concurrency, SQLite, migrations, WebSocket, IRC, OAuth,
   GraphQL, HTMX, Tailwind, `html/template`, charting, accessibility, responsive design, performance,
   regression testing, observability, Docker, CI, PR review, technical writing, and handoff.
2. **Shortlist, read in full.** Anything that hit one of those concerns — or that looked like it might
   cover a lane no installed skill holds — was opened and read completely, together with its dependency
   closure (every `references/`, `scripts/`, `agents/` and `assets/` file it points at). Thirty skills were
   read this way: the 5 installed, plus the 25 candidates recorded with a verdict and a reason in the
   manifest's `excluded_skills[]`.
3. **Material-addition bar.** A shortlisted skill was installed only if it added something the
   already-installed mattpocock, anthropic, Compound Engineering and Trail of Bits sets do not already
   cover — not merely if it was good. Most exclusions below are duplication findings, not quality
   judgements.

The honest limitation of this method: a skill whose directory name and description gave no signal against
that concern list was never opened. If upstream has a genuinely useful Go/SQLite/concurrency skill filed
under an opaque name with a vague description, this sweep would have missed it. Re-sweeping is therefore a
required step of the update procedure below, not an optional one.

Three gaps the sweep confirmed rather than closed, recorded so nobody re-runs the search expecting a
different answer:

- **SQLite and schema migrations** — nothing in this provider covers either. `sql-code-review`, the only
  candidate, documents PostgreSQL/MySQL/SQL Server/Oracle and returns zero hits for `sqlite`.
- **Go architecture and concurrency** — the sweep's Go/concurrency lane came back empty.
  `cloud-design-patterns` was investigated specifically to fill it and does not: it is Azure Architecture
  Center summaries that state outright they cover *why*, not *how*, with nothing about goroutine lifecycle,
  `context` cancellation or race detection.
- **Browser performance profiling** — `chrome-devtools` is the only candidate and is on HOLD (below).

## Installed: 5 skills

- **`github-actions-hardening`** — a security reviewer for `.github/workflows/*.yml` that reasons about the
  Actions threat model rather than application code: `${{ }}` script injection, `pull_request_target` /
  `workflow_run` privilege escalation, SHA-pinning of third-party actions, least-privilege
  `GITHUB_TOKEN` scopes, `GITHUB_ENV`/`GITHUB_OUTPUT` injection, OIDC over long-lived credentials. This repo
  has two live workflows (`.github/workflows/ci.yml`, `release.yml`) and no other installed skill covers
  Actions-specific trust boundaries — the Trail of Bits set owns code-level security depth, which is a
  different thing. 6 upstream files (`SKILL.md` + 5 references).
- **`github-actions-efficiency`** — the cost side of the same surface: caching, concurrency groups, path
  filters, matrix reduction, wasted runs. Notable for degrading honestly when tooling is missing — with no
  shell or `gh` access it asks for pasted workflow files and prefixes its answer with
  "**Static-only analysis** (not confirmed with live runs)", which matters here because this project will
  not trigger a workflow to verify a recommendation (see "Governance precedence"). 5 upstream files
  (`SKILL.md` + 4 references).
- **`harness-engineering`** — turns repeated coding-agent mistakes into durable repository artifacts:
  agent instructions, enforceable checks, failure memory, drift checks, and an adoption report. It is the
  only installed skill aimed at *this repo's own governance surface* as a product. A single 227-line
  `SKILL.md`, no references, no scripts. Its own instruction at `SKILL.md:94` —
  "If the repository already has an equivalent location, update it instead of creating a parallel system" —
  is what makes it safe to install unpatched here (see "Known limitations").
- **`threat-model-analyst`** — whole-system STRIDE-A threat modeling with DFDs, trust boundaries, a
  prioritized findings set and an executive assessment, in two modes (single analysis, and incremental
  against a prior report). The largest install in this set: 17 upstream files including 9 report skeletons,
  a verification checklist and a TMT element taxonomy. Complements rather than duplicates the Trail of Bits
  slice, which works at function/dataflow level.
- **`web-design-reviewer`** — the accessibility and responsive-design audit rubric the installed
  `frontend-design` (aesthetic direction) and `webapp-testing` (browser driving) do not provide, and it
  states thresholds rather than intentions: 4.5:1 body-text contrast
  (`references/visual-checklist.md:66`), 44×44px touch targets (`:92`), `prefers-reduced-motion` support
  (`:187`), and a four-viewport matrix — 375 / 768 / 1280 / 1920 (`SKILL.md:146-153`). It also carries
  Tailwind-specific fixes (`references/framework-fixes.md:87`), which matches this dashboard's stack. It is
  browser-tool-agnostic, so it composes with `webapp-testing` for the actual screenshots. 3 upstream files
  (`SKILL.md` + 2 references).

## Excluded / Held

Twenty-five candidates were read in full and rejected: **23 `EXCLUDE`, 2 `HOLD`**. Every one carries its
own reason in `awesome-copilot-skills-manifest.json`'s `excluded_skills[]`; the categories are:

- **Duplicates of stronger installed skills** — `bug-reproduction-brief` (a subset of `diagnosing-bugs`
  phases 1-2), `agentic-eval`, `ai-team-orchestration`, `doc-and-modernize`, `documentation-writer`,
  `ui-screenshots`, `security-review` (which additionally collides with the Claude Code built-in of the
  same name, so it could only be installed under a rename).
- **Wrong stack or wrong platform** — `sql-code-review` (no SQLite), `cloud-design-patterns`,
  `multi-stage-dockerfile` (this repo's `Dockerfile` already does everything it recommends, and its
  `USER`/Alpine/`HEALTHCHECK` advice is inapplicable to a `scratch` image), `premium-frontend-ui` (GSAP
  scroll narratives and glassmorphism against a `go:embed`-only HTMX dashboard),
  `architecture-blueprint-generator` (unconverted prompt file; Go absent from its stack enum).
- **Governance boundaries** — `github-release` (its core loop *is* release/tag), `dependabot` (its
  reference material instructs `gh pr merge --auto --squash`), `codeql` (see below), `gh-attach` (installs
  an unpinned third-party `gh` extension and authenticates with a raw browser session cookie the skill
  itself calls "a full account credential").
- **Broken or unusable as written** — `quality-playbook` (its mandated `python -m bin.reference_docs_ingest`
  entry point does not exist anywhere in the clone at this pin), `scoutqa-test` (dispatches runs to a
  hosted service whose remote browser cannot reach this project's localhost dashboard),
  `pr-screenshots`, `agent-supply-chain`, `incident-postmortem`.

Two of those merit being singled out because **two independent audit lanes reached opposite verdicts**, and
the manifest records the evidence that decided each:

- **`anti-ui-slop` — EXCLUDED.** One lane said install, one said exclude. Deciding evidence, verified
  directly against the pin: the skill's differentiating input is browsing the third-party hosted catalogue
  at `https://uizze.com` (`SKILL.md:10` — "Browse 800,000+ real web and iOS screens at https://uizze.com
  before choosing a layout" — and `SKILL.md:29`), and its own stated fallback when that catalogue is
  unavailable is to "continue from repository evidence". A capability whose fallback is "do it without the
  capability" is a vendor dependency, not a capability. Its section 5 Finish Gate (responsive behaviour,
  keyboard navigation, focus visibility, contrast, touch targets) is covered *with concrete thresholds* by
  the installed `web-design-reviewer`, and its anti-generic-design thesis is the installed
  `frontend-design`'s repo-pinned role. Excluded as a role duplicate carrying an external vendor
  dependency — not merely for overlapping.
- **`acquire-codebase-knowledge` — EXCLUDED.** Again one lane said install, one said exclude. Deciding
  evidence: its Output Contract is not advisory — `SKILL.md:26` requires that "Exactly these files exist in
  `docs/codebase/`: `STACK.md`, `STRUCTURE.md`, `ARCHITECTURE.md`, `CONVENTIONS.md`, `INTEGRATIONS.md`,
  `TESTING.md`, `CONCERNS.md`" before the skill may finish. `docs/agents/domain.md` commits this project to
  a single-context layout: `CONTEXT.md` at the repo root plus ADRs under `docs/adr/`. Installing it would
  invite a parallel, competing documentation tree against a documented repo decision. Its Go-awareness is
  real — its `scan.py` handles `go.mod`, `cmd/*/main.go` and `.golangci.yml`, which is more than most
  candidates in this corpus offered — and the pro-install lane was right about that. The structural
  conflict decided it, and the onboarding/mapping role is already held by `wayfinder` and `domain-modeling`.

**`codeql` was excluded here specifically because Trail of Bits' `codeql` is installed.** Same directory
name, two providers; only one can own `.claude/skills/codeql`. The Trail of Bits version was judged
stronger for this repo. The awesome-copilot version's Go support is genuine (its
`references/compiled-languages.md` documents `autobuild`, `go.mod` detection and
`CODEQL_EXTRACTOR_GO_OPTION_EXTRACT_TESTS`), so this is a duplication-and-governance exclusion, not a
relevance one — its two primary paths also run straight through non-delegable prohibitions (enabling
default setup is a *repository settings* change, and an advanced-setup workflow only pays off when someone
*runs* it).

### Held, not excluded

Two candidates are `HOLD`: real, non-duplicated capability, blocked by something specific. Neither is
installed, and neither becomes installed by anyone deciding it "seems fine now" — an unblock is a
re-vendor through the update procedure below.

- **`chrome-devtools`.** *What it would add:* browser performance profiling — `performance_start_trace`,
  `performance_analyze_insight`, LCP/CLS insights, CPU and network throttling emulation, and
  `list_network_requests` for the dashboard's HTMX partial round-trips and SSE status stream. The installed
  `webapp-testing` covers navigation, screenshots and console logs, but not tracing or network analysis, so
  this is a genuine gap. *What blocks it:* the entire file (97 lines) is a tool catalogue for the
  `chrome-devtools` MCP server — every workflow pattern calls MCP tool names, and the skill provides no
  fallback, no install instructions and no non-MCP path. That server is not configured for this repo.
  *What would unblock it:* configure the `chrome-devtools` MCP server (pinned to a version, not `@latest`)
  and confirm it can reach a locally-served dashboard; the skill is then usable verbatim with no patch. If
  the owner does not want another MCP dependency, the profiling gap is better closed by a small first-party
  skill than by vendoring a catalogue for a server that isn't there.
- **`agent-skill-stack`.** *What it would add:* a methodology for discovering, hard-gating, ranking,
  conflict-checking and staged-installing skills — precisely the work this audit did by hand, and something
  no installed skill covers. Its strongest single asset is `scripts/inventory_skills.py`, a genuinely
  read-only scanner (verified: stdlib imports only — `argparse`, `json`, `os`, `re`, `pathlib`, `typing`;
  the `curl`/`urllib`/`subprocess` strings in it are *detection patterns*, not calls) that walks skill
  roots, parses frontmatter, computes pairwise trigger-description Jaccard overlap and flags eight
  risk-indicator classes. With 81 installed skills across six providers, recall collision between skill
  descriptions is a real gap that this repo's own validator does not fill — it checks manifests, blob
  hashes and frontmatter keys, not routing overlap. *What blocks it:* both ends of the skill are wrong for
  this repo as written. Its discovery backbone is network and unpinned (`npx skills find <query>`,
  skills.sh, registries, OpenCLI web discovery), and its index/install roots are Codex/Hermes paths
  (`~/.codex/skills`, `~/.codex/plugins/cache`, `.codex/skills`, `~/.agents/skills`, `~/.hermes/skills`).
  Its `scripts/stage_install.py --dest <skill root>` writes a skill directory plus a lock manifest into a
  skill root — which is exactly the act that Governance v3 vendoring owns (manifest entry, upstream and
  vendored blob pins, policy plus ledger, dedicated Draft PR). *What would unblock it:* either (a) vendor
  it with its discovery and install steps narrowed by patch to "read-only inventory of
  `.claude/skills/**`, report only" — a patch large enough that it deserves its own reviewed PR and a
  conscious decision that it is still minimal patching rather than a rewrite; or (b) don't vendor it, and
  port just the overlap scan into `scripts/validate-agent-governance.py` as a first-party check. Option (b)
  is the smaller change and does not import an install flow that competes with this project's own.

## Invocation modes

All five installed skills are **model-invoked**, exactly as upstream ships them. No skill in this set is
renamed (`renamed_from: null` throughout) and none carries `disable-model-invocation`.

`threat-model-analyst` is effectively self-limiting without any local change: its own upstream description
ends "Only activate when the user explicitly requests a threat model analysis, incremental update, or
invokes /threat-model-analyst directly." That is upstream's text, not a patch — worth stating so nobody
looks for a ledger row that explains it.

## License & attribution

All five vendored skills are **MIT**, `Copyright GitHub, Inc.` MIT requires that "the above copyright notice
and this permission notice shall be included in all copies or substantial portions of the Software", so
upstream's root `LICENSE` (blob `89bc5e962c9944cdb050887062afdaaf89be504a`) is copied **verbatim into every
vendored skill directory** — `.claude/skills/<name>/LICENSE`, five identical copies, each hash-verified in
the manifest. `provider-license-files` enforces the per-skill layout.

- `.claude/skills/LICENSE` is the *Matt Pocock* set's shared MIT notice and does **not** cover this set.
  Two MIT upstreams, two separate attributions.
- MIT imposes no Apache-§4(b)-style "mark your modified files" obligation, but the same marker-plus-ledger
  convention is applied here anyway — for reviewability, and because
  `provider-patch-marker-coverage` requires every in-file marker id to appear both in this provider's
  ledger and in some `files[].patch_ids`.
- This repository is **GPL-3.0** (root `LICENSE`). MIT is one-way compatible into a GPLv3 work — MIT code
  may be included in a GPLv3-licensed project, the reverse is not true — and the MIT notice must still
  accompany the copies, which is what the per-skill `LICENSE` files do.

## Local patches summary

**There are no local patches.** All 37 vendored files — 32 upstream-origin plus 5 verbatim `LICENSE`
copies — are byte-identical to upstream commit `a80885b76044550770f60f360f8a0e5ae3524a31`. The manifest
records `locally_modified: false` for every file, and `provider-file-hashes` verifies each one against a
read-only clone at that commit when `GOVERNANCE_UPSTREAM_DIR_AWESOME_COPILOT` is set.

One patch was made during vendoring and then **withdrawn**, and the reasoning is recorded in
`docs/agents/awesome-copilot-skills-patches.md` because it is reusable. `relative-links-resolve` had
reported nine dangling links in `threat-model-analyst/references/output-formats.md` — links to documents
the skill *generates in the target repo*, not to vendored files. Investigating it while writing the ledger
showed the defect was in the **checker**: `strip_code_fences()` paired fences with a regex that cannot
express CommonMark's rule that a fence closes only on the same character, at least as long as the opener,
so the file's nested four-backtick fence was mis-paired and its specimen tables escaped the stripper. The
validator was fixed (line-based, fence-length-aware; self-test `P16`, mutation-verified), after which the
check passes against the unmodified upstream file, and the patch was reverted.

The rule that follows, and the reason it is stated here rather than buried: a validator finding that can
only be cleared by editing correct upstream prose should be treated as a suspected defect in the validator
first. That pressure is precisely what this policy exists to remove.

The id named by this provider's convention, `ghac-mode-normalize`, has **zero rows** — every
upstream-origin file in the installed set is already `100644` at the pin. See "Installation model".

### Default: minimal patching

Under Governance v3 (`docs/adr/0002-governance-v3-skill-native-orchestration.md`), skills are preserved as
close to their authors' intent as practical. **Do not patch a skill merely because it uses subagents,
several writers, reviewers/critics, parallel analysis, iterative fixes, or its own handoff/orchestration
pattern.** That is engineering workflow, and workflow belongs to the skill (see
`docs/agents/agent-orchestration.md`). Patch only for concrete project incompatibility, a broken dependency,
license/provenance necessity, or a genuine authority/integrity boundary.

This set is the clearest illustration of that rule in practice, because the most orchestration-heavy skill
here was left completely untouched. `threat-model-analyst/references/orchestrator.md` ships a mandatory
"⛔ Sub-Agent Governance" section (`:87`) with five rules — the parent agent owns *all* file creation,
sub-agents are read-only helpers with fresh context windows, sub-agent prompts must be narrow, the parent
owns the output folder path, with one narrow exception for `threat-inventory.json` — plus a delegated
verification phase (`:20`, "Delegate to a sub-agent and include `verification-checklist.md` in the
sub-agent prompt") and a re-run-until-clean repair loop (`:531`). Every byte of that is preserved. It is a
well-specified multi-agent methodology with deterministic write ownership, which is exactly what the skill
is entitled to own.

Two other candidates for "obvious" patches were also deliberately not patched:

- `web-design-reviewer` recommends Playwright MCP as its reference implementation and shows an
  `npx -y @playwright/mcp@latest` config block (`SKILL.md:291-311`). That server is not configured here and
  is not adopted; browser evidence comes from the installed `webapp-testing`. The skill is
  browser-tool-agnostic in substance, so the recommendation was left in rather than edited out — noted
  under "Known limitations" instead.
- `harness-engineering` proposes writing into `.github/copilot-instructions.md`, `docs/decisions/` and
  `docs/failures/`. Those paths are not this repo's layout, but the skill's own instruction is to update an
  existing equivalent rather than create a parallel system, and the mechanical boundary is already held by
  `.claude/settings.json` and the hook — not by rewriting a skill's prose.

## Governance precedence

Vendored skills sit **below** this project's own policy **on authority**. The authority chain has exactly
four levels (see `CLAUDE.md`'s "Claude Code Governance (v3)" section and
`docs/agents/agent-orchestration.md`), narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched, this set alongside every other.
4. **Generic model behavior** — fallback only.

Unpatched upstream text is **not** a fifth tier below the patches: a vendored skill's instructions are
whatever its vendored bytes say, patched and unpatched alike, all together at level 3. Where a local patch
and the upstream text around it disagree, the patch wins — that is what patching means, and it is resolved
inside level 3. A skill instruction never overrides a `.claude/rules/*.md` constraint or a hook denial.

**On workflow the order is inverted**: an invoked audited skill owns its documented engineering
methodology — agents, lanes, reviewers, writers, repair loops — and the project does not override it. See
`docs/agents/agent-orchestration.md`, and the `threat-model-analyst` example above.

The non-delegable prohibitions bind every agent at every delegation depth, whatever a skill's own text
says: no marking a PR ready for review, no merge or auto-merge (**the owner performs merges**), no
release/tag/deploy, no triggering or rerunning a GitHub Actions workflow, no GitHub settings or secrets
changes, no force push, no direct push to `main`/`master`. This provider makes that boundary unusually
concrete — three of its skills (`github-release`, `dependabot`, `codeql`) were excluded because their core
loops cross it, and the two Actions skills that *were* installed are useful precisely as far as authoring
and reviewing workflow YAML, and no further: recommendations are static analysis, never verified by
dispatching a run.

## Supply-chain assumptions

Same rationale as the other five providers: a live install trusts upstream's default branch on every future
run, not just the commit reviewed today. Vendoring converts that into "trust as of a specific reviewed SHA,
re-established only when someone deliberately re-reviews."

This provider's manifest uses the **file-level** schema: every `files[]` entry carries both an
`upstream_blob_sha` (what upstream had at the pin) and a `vendored_blob_sha` (`git hash-object` of the file
as committed here). `provider-file-hashes` fails closed on any on-disk edit to any vendored file — patched,
unpatched, or local-origin — that isn't accompanied by a deliberate `vendored_blob_sha` bump in the
manifest. That is the re-audit forcing function: you cannot quietly edit a vendored skill and have the
validator stay green. With `GOVERNANCE_UPSTREAM_DIR_AWESOME_COPILOT` pointing at a read-only clone at the
pin, `upstream_blob_sha` is additionally verified against upstream itself, so divergence *from upstream* is
detected rather than mere self-consistency.

The installed slice is unusually low-risk on content: 32 Markdown files, zero executables, zero network
calls of its own. The residual supply-chain surface is what the *instructions* would have an agent reach
for, and it was reviewed on that basis:

- `web-design-reviewer` recommends an MCP server installed via `npx -y @playwright/mcp@latest` — unpinned,
  and not configured or adopted here (see "Known limitations").
- `github-actions-efficiency` expects `gh` for live run data; the commands it names are reads
  (`gh run list`), and it has a documented static-only degradation path when `gh` is unavailable.
- Several *excluded* candidates were rejected on exactly this axis, which is why they were read in full
  rather than skimmed: `gh-attach` (unpinned third-party `gh` extension plus a full-account session
  cookie), `scoutqa-test` (dispatches to a hosted third-party runner), `anti-ui-slop` (third-party hosted
  catalogue), `chrome-devtools` (unconfigured MCP server).

What was *not* done, stated plainly: no test suite, static analysis scan or dynamic execution was run
against this provider's skills — there is no executable content in the installed set to run or scan. The
review consisted of reading the shortlisted skills and their closures, diffing the vendored tree against
the pinned clone, and recording hashes.

## Update procedure

1. Fetch the new upstream commit into a read-only clone (never edit it in place); set
   `GOVERNANCE_UPSTREAM_DIR_AWESOME_COPILOT` to that clone's path for the validator's stricter blob-hash
   mode.
2. Diff the set of directories under `skills/` against the last-reviewed list — additions, removals and
   renames among the five installed skills, **and** whether any of the 25 recorded candidates changed in a
   way that alters its verdict (especially the two `HOLD` entries).
3. **Re-sweep, don't assume.** The corpus grows; the last sweep covered 408 directories by name and
   description. Sweep the delta the same way, against the same concern list in
   "Selection: how the 408-skill corpus was swept", and read in full anything that hits. Record every new
   candidate in `excluded_skills[]` with a reason and an `EXCLUDE`/`HOLD` verdict, even the easy nos —
   the value of that list is that the next reviewer doesn't repeat the work.
4. For each vendored skill, diff every file in its `files[]` list against the **currently-vendored copy**
   (not raw upstream — `output-formats.md` is patched) to separate genuinely new upstream content from the
   existing patch.
5. Re-run the same review judgment on anything new: does it assume background execution, external network
   fetch, credentials, an unconfigured MCP server, or a write into a git working tree this project doesn't
   grant? If so, patch it minimally and mark it — the test is **authority**, not orchestration. A skill's
   agent topology, fan-out width, writer count, reviewer lanes and repair loops are never grounds for a
   patch (see "Default: minimal patching").
6. Update `upstream_commit`, `upstream_tree`, `upstream_current_head`, `drift` and `reviewed_at` in
   `awesome-copilot-skills-manifest.json`; update every touched file's `upstream_blob_sha`/`upstream_mode`
   and `locally_modified`/`patch_ids`. Recompute `vendored_blob_sha` (`git hash-object <path>`) for every
   touched file **last**, after all edits for the round are finalized — that is what re-pins the file and
   clears the fail-closed check.
7. Update `awesome-copilot-skills-patches.md` for any patch added, changed or removed, and this policy's
   "Installed" / "Excluded / Held" / "Selection" sections if the set or the sweep changed.
8. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
9. Open the change as its own dedicated Draft PR (see `mattpocock-skills-policy.md`'s "Dedicated Draft PR
   requirement" — the same rule applies here); never bundle a skills re-vendor into an unrelated change.
   Get human review before merge; this policy forbids the agent from merging it.

## Rollback

1. Identify the last-known-good `upstream_commit` from `awesome-copilot-skills-manifest.json`'s git history.
2. Restore the five affected directories from that commit: `git checkout <sha> --
   .claude/skills/github-actions-efficiency .claude/skills/github-actions-hardening
   .claude/skills/harness-engineering .claude/skills/threat-model-analyst
   .claude/skills/web-design-reviewer`.
3. Restore `docs/agents/awesome-copilot-skills-manifest.json` and `awesome-copilot-skills-patches.md` from
   the same commit (and this policy, if it moved with them).
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description.

Rolling back this provider is independent of the other five: no other manifest claims any of those five
directories, and no cross-provider patch id exists.

## Known limitations

- **`web-design-reviewer` permits remote targets in its own prose.** Its Prerequisites list
  "Staging environment" and "Production environment (for read-only reviews)" (`SKILL.md:22-25`). That text
  is unpatched. The localhost-only constraint on this project comes from `CLAUDE.md`, from
  `.claude/rules/*.md`, and from the installed `webapp-testing`'s `webapp-testing-localhost-only` patch —
  the skill that actually drives the browser — not from this skill's text. Since `web-design-reviewer`
  supplies criteria rather than a browser, that layering holds; but if it is ever paired with a different
  browser tool, the target restriction has to come from somewhere else.
- **Its recommended reference implementation is an unconfigured MCP server.** Playwright MCP via
  `npx -y @playwright/mcp@latest` (`SKILL.md:291-311`) is unpinned and not installed here. Anyone following
  that section literally will find no such tools. Use `webapp-testing` for the browser half.
- **`threat-model-analyst` writes a multi-file report tree into the working directory.** Its orchestrator
  creates a timestamped output folder and writes `0-assessment.md`, `0.1-architecture.md`,
  `1-threatmodel.md`, `2-stride-analysis.md`, `3-findings.md`, `.mmd` diagram sources and
  `threat-inventory.json`. The `ghac-generated-output-links` patch changes how those filenames *render* in
  the reference doc; it does not change where the skill writes. Output location is governed by the active
  task contract and operation mode, as with any other writing skill.
- **`harness-engineering` proposes a layout this repo does not use.** Its artifact table names
  `.github/copilot-instructions.md`, `docs/decisions/*.md` and `docs/failures/*.md`; the equivalents here
  are `CLAUDE.md` plus `.claude/rules/*.md`, ADRs under `docs/adr/` (per `docs/agents/domain.md`), and
  `scripts/validate-agent-governance.py` as the drift check. Its own principle says to prefer the existing
  location, which is why it is installed unpatched — but "it proposed a parallel governance tree" is the
  specific thing to catch when reviewing its output. Its table also offers "a small new workflow" for CI
  enforcement: `.github/workflows/**` changes require explicit `ask` confirmation even under an active
  contract (`.claude/settings.json`), and nothing may trigger or rerun the result.
- **Both Actions skills stop at authoring and review.** Neither may verify a recommendation by dispatching
  or rerunning a workflow. `github-actions-efficiency`'s cost estimates against this repo are therefore
  static unless the owner supplies real run data.
- **The sweep is a sweep.** 408 directories screened by name and description; 30 read in full. A useful
  skill hidden behind an opaque name and a vague description would have been missed. See step 3 of the
  update procedure.
- **The hook/permission layer is a backstop, not a substitute for reading a skill.** As
  `mattpocock-skills-policy.md`'s "Known limitations" section notes, a sufficiently subtle instruction can
  still shape agent *reasoning* even where it cannot force a blocked tool call. That caveat applies to this
  set identically and is not repeated in detail here.

## Compatibility

Reviewed and vendored against the same Claude Code version and governance-layer assumptions recorded in
`mattpocock-skills-policy.md`'s "Compatibility" section (frontmatter, permission and hook-payload
behavior) — see that document rather than duplicating the version pin here; re-verify them together if
either changes. One provider-specific note: these skills were authored for GitHub Copilot, so a future
upstream may introduce Copilot-only frontmatter keys. This provider's `extra_frontmatter_keys` allowlist is
currently empty, which means any such key fails the frontmatter check rather than being silently accepted —
that is the intended behavior, and widening the allowlist is a reviewed decision, not a fix.
