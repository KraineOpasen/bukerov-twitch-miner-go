# Trail of Bits skills — vendoring policy

## Purpose

This project vendors a reviewed, audited subset of
[trailofbits/skills](https://github.com/trailofbits/skills) into `.claude/skills/` instead of installing it
as a live Claude Code plugin marketplace. This document is the policy for what's installed, why, how it's
patched, and how to update it. See also `docs/agents/trailofbits-skills-manifest.json` (machine-readable
inventory, file-level) and `docs/agents/trailofbits-skills-patches.md` (per-patch ledger).

This is one of six independent vendored sets (`mattpocock`, `anthropic`, `compound-engineering`,
`trailofbits`, `awesome-copilot`, `builderio`); each has its own upstream, manifest and ledger, and no two
may claim the same directory under `.claude/skills/` — enforced by
`scripts/validate-agent-governance.py`'s `manifest-ownership-partition` check. That partition matters
concretely here: `github/awesome-copilot` also ships a `codeql` skill, and `.claude/skills/codeql` is owned
by **this** provider (see that set's policy for why its own version was excluded).

This set is also, by some distance, the largest and the most orchestration-heavy of the six. That is the
point of it: it brings security-audit methodology — context building, differential review, false-positive
verification, variant analysis, spec compliance, mutation triage and code-graph analysis — that no other
installed provider covers. Its subagent topologies were preserved deliberately (see
"Default: minimal patching").

## Upstream

- Repo: `https://github.com/trailofbits/skills`
- Reviewed commit: `04b241176fd9c10738a61df53d2c677c53e42990` — advanced from
  `4db88ee79db0a68bbe049fe827e272ee2bc19510` by the audited re-vendor for Issue #177 (one true
  fast-forward commit, upstream #235, which reworks the `property-based-testing` skill; the other 22
  installed subtrees are byte-identical at both pins)
- Current upstream HEAD at publication of that re-vendor: `07bce8a2c8ccc56c5b44b7067a04b8bf46128f05`
  (**drift: ahead of pin, not audited** — a 9-commit fast-forward that touches none of the 23 installed
  subtrees and adds a new sibling plugin, `goal-prompt`, deliberately left to the update bot's own
  discovery schedule; see the manifest's `drift` field for the full record)
- Reviewed tree SHAs: one per vendored skill directory, all 23 recorded in the manifest's `upstream_tree`
  map rather than duplicated here — a 23-entry hash table is a copy-drift hazard in prose, and the
  validator reads the manifest, not this document.
- Corpus size at the pin: **40 plugins containing 78 skill directories**, of which **23** are installed
  here.

The structural fact that shaped everything below: upstream does not ship skills as skills. It ships
**Claude Code plugins**, laid out as `plugins/<plugin>/skills/<skill>/`, with plugin-level `commands/`,
`workflows/`, `agents/` and `.claude-plugin/plugin.json` alongside. A plugin can carry several skills —
`trailmark` supplies 8 of the 23 installed here and `static-analysis` another 3 — and several of the best
skills lean on machinery that lives *outside* their own directory. A project-local vendored install gets
the skill directory and nothing else, so each candidate had to be checked for whether its dependency
closure survives that, not just for whether its content is good. Two candidates were rejected on exactly
that ground (`semgrep-rule-variant-creator`, `slicing-code-context`), one is on HOLD because of it
(`insecure-defaults`), and five installed skills needed the structural adaptations described next.

## Installation model

**Project-local vendored copy**, not a live plugin install. Each skill's files are copied verbatim into
`.claude/skills/<name>/` at review time, then minimally patched (see below), and every file's mode is
normalized to `100644` — no executable bits anywhere under `.claude/skills/**`
(`no-symlinks-no-exec-under-claude`). `automatic_updates: false` — nothing about this installation
re-fetches or re-syncs from upstream on its own. A human, or an explicitly-contracted agent task, must
re-run the review process to move the pin.

The vendored set is **204 files: 181 upstream-origin plus 23 local-origin `LICENSE` copies** (one per skill
directory, see "License & attribution"). Six of the 23 skills ship executable content — `codeql`,
`semgrep`, `sarif-parsing`, `diagramming-code`, `graph-evolution` and `supply-chain-risk-auditor` — and
those six carry `scripts_audited: true` in the manifest, meaning every `.py` and `.sh` file in their
`files[]` was read end to end during this review, not merely diffed. The other seventeen carry `null`:
there was no executable content to audit, not an audit that was skipped.

### Two structural adaptations

Both are consequences of installing a plugin's skill *as a skill*. Neither is a judgement about upstream's
content.

**1. The skill-level `agents/openai.yaml` was dropped from every skill that had one.** Twenty-two of the 23
installed skills ship `plugins/<plugin>/skills/<skill>/agents/openai.yaml`, and in all 22 that directory
contains **that file and nothing else**. It is not an agent definition. It is Codex marketplace interface
metadata — four lines of `icon_small`/`icon_large` pointing at `assets/trail-of-bits-mark.svg` plus
`brand_color: "#D83A34"` — identical across all 22 (blob `1d437b6dfffe6d157d6744ea946a9c9620578c2a`).
`openai.yaml` is additionally a forbidden vendored filename in this repository
(`FORBIDDEN_VENDOR_NAMES` in `scripts/validate-agent-governance.py`, checked by
`forbidden-vendor-files-absent`), alongside `.github`, `.claude-plugin` and the npm manifests. The file is
a deletion, so it can carry no in-file marker; it is recorded as a row in the ledger instead. The
twenty-third skill, `vulnerability-triage-brocards`, ships no `agents/` directory at all (and no `assets/`
either — it is three files: `SKILL.md`, one reference, and the added `LICENSE`).

**2. Seven real Claude Code subagent definitions were relocated from plugin root into their skill's own
`agents/` directory.** Upstream keeps them at `plugins/<plugin>/agents/*.md`, outside the skill directory —
which is correct for a plugin install and invisible to a project-local one. Dropping them would have left
five installed skills instructing an agent to dispatch a subagent whose definition does not exist in the
tree. The seven, by skill:

| Skill | Relocated agent file | Upstream location |
| --- | --- | --- |
| `audit-context-building` | `function-analyzer.md` | `plugins/audit-context-building/agents/` |
| `differential-review` | `adversarial-modeler.md` | `plugins/differential-review/agents/` |
| `fp-check` | `data-flow-analyzer.md` | `plugins/fp-check/agents/` |
| `fp-check` | `exploitability-verifier.md` | `plugins/fp-check/agents/` |
| `fp-check` | `poc-builder.md` | `plugins/fp-check/agents/` |
| `sharp-edges` | `sharp-edges-analyzer.md` | `plugins/sharp-edges/agents/` |
| `spec-to-code-compliance` | `spec-compliance-checker.md` | `plugins/spec-to-code-compliance/agents/` |

Four of the seven needed a one-segment path edit and carry the `tob-plugin-agents-relocated` id (see
`{baseDir}` below). `fp-check`'s three are **byte-identical to upstream** — they reference no intra-skill
paths at all — so they are recorded with `relocated_from_plugin_root: true` and empty `patch_ids`, and the
move itself is what the ledger's relocation row documents.

One plugin-root agent was deliberately **not** relocated: `plugins/trailmark/agents/code-slice-worker.md`.
Its only consumer is `slicing-code-context`, which is excluded, so vendoring it would have added an
orphaned agent definition no installed skill dispatches.

### `{baseDir}`

Trail of Bits writes intra-skill paths as `{baseDir}/references/foo.md`. This is **their own documented
convention** for "this skill's own directory" — upstream `AGENTS.md:132`: "Use `{baseDir}` for paths,
**never hardcode** absolute paths" — not a variable any loader substitutes. The vendoring keeps the token
verbatim, in hundreds of lines across the set, and `scripts/validate-agent-governance.py` **resolves** it
against the skill root instead (`resolve_skill_link`, exercised by the validator's own `P15` self-test).
Rewriting those lines to strip the token would have been a large, content-touching patch made solely to
satisfy a link checker — precisely the kind of change this policy exists to prevent — and resolving it in
the validator makes the check stronger, because those targets are now genuinely verified rather than
skipped.

The relocated agent files are the one place where the token needed help: written from plugin root, they
said `{baseDir}/skills/<skill>/resources/...`. Vendored into the skill directory, that segment is one level
too deep, so it was dropped — `{baseDir}/resources/...`. That single, mechanical edit is
`tob-plugin-agents-relocated`. After it, no vendored file in this set contains the string
`{baseDir}/skills/` (verified: zero occurrences across `.claude/skills/`).

### Script invocation and file modes

Every Python entry point in this set is invoked through an explicit interpreter in the skills' own
prose — `uv run {baseDir}/scripts/diagram.py`, `uv run {baseDir}/scripts/merge_sarif.py`,
`uv run {baseDir}/scripts/collect.py`, and so on. Three shell scripts were `100755` upstream and are
`100644` here (`tob-mode-normalize`): `codeql/scripts/find_databases.sh`,
`codeql/scripts/generate_suite.sh`, `semgrep/scripts/run-scans.sh`. Their content is untouched — each
file's `upstream_blob_sha` still matches on disk. Upstream prose invoked those three scripts by bare path
at six call sites, every one of which would fail with `EACCES` at mode `100644`; all six are prefixed with
`bash` under `tob-exec-bit-interpreter`, and the reasoning is recorded under "Known limitations".

## Installed: 23 skills

Grouped by the upstream plugin they came from, since that is where their siblings and their excluded
neighbours live.

**`trailmark` plugin — code-graph analysis (8 skills).** The largest single family here, all built on the
`trailmark` CLI.

- **`trailmark`** (5 files) — builds and queries multi-language source and binary code graphs: blast
  radius, taint propagation, privilege boundaries, entry points, structural traversal, graph diffs, SQL
  schema graphs. The base capability the other seven compose on.
- **`trailmark-finding-triage`** (6) — graph-assisted triage of a *single* finding: is it reachable, does
  an entrypoint path exist, what is the blast radius.
- **`trailmark-review-gate`** (6) — structural review gate over a branch, PR or ref range: new entrypoints,
  new tainted paths, removed validation or authorization calls, privilege-boundary drift.
- **`trailmark-variant-neighborhood`** (6) — expands one confirmed bug into a graph neighbourhood of
  variant candidates (siblings, shared callers, common sinks, interface implementations).
- **`graph-evolution`** (6) — compares graphs at two snapshots to surface structural changes a text diff
  misses. Ships `scripts/graph_diff.py`.
- **`diagramming-code`** (6) — Mermaid diagrams from a code graph: call graphs, class hierarchies, module
  dependency maps, complexity heatmaps, attack-surface data flow. Ships `scripts/diagram.py`.
- **`audit-augmentation`** (4) — projects SARIF results, weAudit annotations and binary-analysis findings
  onto a graph, mapping findings to nodes by file/line overlap.
- **`genotoxic`** (6) — graph-informed mutation-testing triage: survived mutants and unnecessary test
  statements crossed against call-graph data to separate false positives from real coverage gaps and
  fuzzing targets. Directly relevant to a Go repo whose test suite runs under `-race` on every package.

**`static-analysis` plugin — scanners and their output (3 skills).**

- **`semgrep`** (9) — full Semgrep scan: language detection, ruleset selection, an explicit approval gate
  before anything runs, batched execution through `scripts/run-scans.sh`, merged SARIF. Two modes
  ("run all", "important only").
- **`codeql`** (34 files, the largest skill in the set) — interprocedural data-flow and taint analysis,
  including Go. Fifteen scripts, eleven reference documents, three workflow documents; its own test suite
  covers the shell and Python helpers.
- **`sarif-parsing`** (5) — parses, filters, deduplicates and converts SARIF that already exists. Runs no
  scan itself, which is exactly why it is separate.

**Single-skill plugins (12).**

- **`audit-context-building`** (7) — understand a codebase before hunting bugs in it: what each function
  assumes, guarantees and depends on. Dispatches one `function-analyzer` subagent per function and writes a
  dossier.
- **`differential-review`** (8) — security-focused differential review of a PR, commit or diff, with blast
  radius, test-coverage checks and an adversarial modelling pass.
- **`fp-check`** (12) — verifies a *suspected* bug to a TRUE/FALSE POSITIVE verdict with documented
  evidence. Three subagents: data-flow analysis, exploitability verification, PoC construction.
- **`variant-analysis`** (18) — after one bug is found, hunts its variants across the codebase, and
  generalizes an instance into a CodeQL or Semgrep query. Ships per-language `.ql` and `.yaml` templates
  including Go.
- **`spec-to-code-compliance`** (8) — checks code against the documents that specify it. Directly usable
  against `SPECIFICATIONS.md`, which this repo treats as normative for auth, API, pubsub, chat, drops and
  bet logic.
- **`sharp-edges`** (20) — finds error-prone APIs, dangerous configuration defaults and footgun designs;
  ships a per-language footgun guide including `references/lang-go.md`.
- **`supply-chain-risk-auditor`** (13) — dependency risk: version-matched advisories, abandoned upstreams,
  install-time script execution. Ships eight Python files with a `pyproject.toml` and `uv.lock`.
- **`property-based-testing`** (9) — writes, reviews and debugs property-based tests across languages
  (Go via `pgregory.net/rapid`); relevant to `internal/models`' bet-strategy and filter-condition logic.
  Reworked upstream at the current pin (#235): tighter SKILL.md with explicit tautology/vacuity failure
  modes and a sharper trigger description with explicit negative boundaries (no fuzzing, mutation
  testing, static analysis, benchmarking or E2E); `references/design.md` and `references/strategies.md`
  were deleted upstream with recorded rationale (`strategies.md`'s decision-level guidance was absorbed
  into `generating.md`; `design.md`'s Property-Driven-Development workflow was retired outright, its
  durable fragments — the property-discovery table, strength ordering and tautology warning — surviving
  as SKILL.md's catalog and failure-mode sections). The upstream frontmatter `effort: low` pin is
  removed here by `tob-effort-pin-dropped` (see "Local patches summary").
- **`mutation-testing`** (5) — scoping and tuning mutation-testing campaigns (mewt, muton).
- **`harness-writing`** (3) — fuzzing-harness technique, language-agnostic. The one skill in the set
  carrying a `type: technique` frontmatter key.
- **`semgrep-rule-creator`** (5) — authoring custom Semgrep rules.
- **`vulnerability-triage-brocards`** (3) — seven rules of thumb for accepting, dismissing or
  requesting-more-information on an incoming vulnerability report.

Routing between these and the other five providers' skills is documented in `docs/agents/skills-routing.md`,
not here.

## Excluded / Held

**Finding a rejected candidate upstream.** `excluded_skills[]` records a candidate by `name` only — its
`upstream_path` is the empty string in all 25 entries, so the manifest alone will not lead you to the
directory. Use the layout convention instead: every upstream skill lives at
`plugins/<plugin>/skills/<skill>/`, so any named candidate is `plugins/*/skills/<name>/` in a clone at the
pin. `<plugin>` is frequently *not* `<skill>` — 14 of the 25 sit under
`plugins/testing-handbook-skills/skills/` and 6 under `plugins/trailmark/skills/`; only
`agentic-actions-auditor`, `entry-point-analyzer`, `second-opinion` and `semgrep-rule-variant-creator` are
in same-named single-skill plugins. The twenty-fifth, `insecure-defaults`, has no `skills/` directory at
all — it is `plugins/insecure-defaults/` with `commands/`, `workflows/`, `references/` and `tests/` and
nothing else, which is exactly why it is held.

The arithmetic, stated plainly because it does not close to a single number without explanation:

- **78** skill directories exist upstream at the pin.
- **23** are installed.
- **25** entries appear in `trailofbits-skills-manifest.json`'s `excluded_skills[]`, each with a reason and
  an `EXCLUDE`/`HOLD` verdict: **23 `EXCLUDE`, 2 `HOLD`**. Twenty-four of those are upstream skill
  directories; the twenty-fifth, `insecure-defaults`, is a *plugin that ships no skill directory at all*,
  which is precisely why it is held.
- That leaves **31 upstream skill directories with no per-candidate verdict.** Stated exactly, because the
  distinction matters: they were ruled out at **plugin** granularity — 21 plugins whose own subject matter,
  as their `plugin.json` and skill descriptions state it, placed them outside the slice — and the skills
  inside them were **not read individually**. Nothing here is a per-skill judgement about any of the 31.
  Every one of the 21 plugins is wholly out: none of them contributed an installed skill or an itemised
  `excluded_skills[]` entry either. The 21, grouped by the ground that ruled them out:
  - **Smart contracts (1 plugin, 11 skills).** `building-secure-contracts`, which its own `plugin.json`
    describes as a "smart contract security toolkit … vulnerability scanners for 6 blockchains and 5
    development guideline assistants": the six scanners are `algorand-`, `cairo-`, `cosmos-`, `solana-`,
    `substrate-` and `ton-vulnerability-scanner`; the five assistants are `audit-prep-assistant`,
    `code-maturity-assessor`, `guidelines-advisor`, `secure-workflow-guide` and
    `token-integration-analyzer`.
  - **Other languages (3).** `c-review`, `rust-review`, `modern-python`.
  - **Cryptographic secret handling (2).** `constant-time-analysis`, `zeroize-audit` — the same ground on
    which `constant-time-testing` is itemised below, not a language ground: `constant-time-analysis` names
    Go among its target languages, but this project implements no cryptographic primitive to leak timing on.
  - **Blockchain arithmetic (1).** `dimensional-analysis`, whose description scopes it to "a DeFi protocol,
    offchain code, or other blockchain-related codebase".
  - **Other artifact and tool domains (4).** `dwarf-expert` (DWARF debug info), `firebase-apk-scanner`
    (Android APKs), `burpsuite-project-parser` (`.burp` project files), `yara-authoring` (YARA-X rules;
    skill `yara-rule-authoring`).
  - **Proof assistants (1).** `writing-lean-proofs` (Lean 4 / Mathlib).
  - **Developer, agent and internal workflow (9).** `devcontainer-setup`, `gh-cli`, `git-cleanup`,
    `github-triage`, `open-sourcing`, `skill-improver`, `claude-in-chrome-troubleshooting` (skill
    `chrome-mcp-troubleshooting`), `culture-index` (skill `interpreting-culture-index`), `let-fate-decide`.

  Plugin granularity is a real cost and it is not hypothetical here. Inside `building-secure-contracts`,
  `audit-prep-assistant` is not itself smart-contract-only: its Step 2 runs `slither`, `dylint` **and**
  `golangci-lint run`, so a Go repository is squarely in its range. It stays unreviewed because its plugin
  was ruled out, which is the honest description of what happened rather than a verdict on the skill. See
  step 3 of the update procedure: this is a scoped review, not an exhaustive one, and this document does
  not claim otherwise.

The 23 exclusions fall into four groups, all reasoned in the manifest:

- **Wrong language or toolchain (11).** The fuzzing family, almost entirely: `address-sanitizer`, `aflpp`,
  `atheris`, `cargo-fuzz`, `libafl`, `libfuzzer`, `ossfuzz`, `ruzzy`, `fuzzing-dictionary`,
  `fuzzing-obstacles`, `coverage-analysis`. These target Clang/LLVM, Cargo, CPython or Ruby; Go has native
  `go test -fuzz` and no instrumentation path without cgo, which this project does not use. Two are worth
  singling out because they were rejected on evidence rather than on the language label:
  `fuzzing-dictionary`'s entire mechanism is the `-dict=` flag, and its own Go section states go-fuzz has no
  dictionary support; `fuzzing-obstacles`' quick-reference table is literally headed "| Task | C/C++ | Rust |"
  and its remedies are `#ifdef FUZZING_BUILD_MODE_UNSAFE_FOR_PRODUCTION` and `cfg!(fuzzing)`, neither of
  which exists in Go. What transfers from both is already covered by the installed `harness-writing`.
- **Cryptographic scope this project does not have (5).** `constant-time-testing`, `crypto-protocol-diagram`,
  `mermaid-to-proverif`, `vector-forge`, `wycheproof`. The miner implements no cryptographic primitive and
  no cryptographic protocol — it consumes Twitch's OAuth device-code flow and relies on stdlib TLS — so
  there is nothing to instrument for timing leaks, annotate as a protocol, or validate against a test-vector
  suite.
- **Wrong platform (1).** `entry-point-analyzer` is smart-contract-only: its language detection dispatches
  solely on `.sol`, `.vy`, Move, FunC/Tact and Solana/CosmWasm Rust, and its own "When NOT to Use" says
  "Non-smart-contract codebases". A Go repo produces zero detected languages.
- **Duplicates, subsets, or broken closures (6).** `trailmark-summary` and `trailmark-structural` are
  strict subsets of the installed `trailmark` (both name an orchestrator — "vivisect", "galvanize" — that
  exists nowhere in the clone at this pin), and installing them would add competing triggers for
  "structural overview" with no added methodology. `testing-handbook-generator` duplicates the already
  installed `skill-creator-anthropic`, and points at a validator script and three exemplar skills that this
  slice excludes. `semgrep-rule-variant-creator` exists to port a rule across languages, which a
  single-language repo never needs, and depends on a 981-line `workflows/port-rule-to-languages.js` at
  plugin root plus a `Workflow` tool and `${CLAUDE_PLUGIN_ROOT}` that a project-local install does not have.
  `slicing-code-context` solves a confidentiality/context-budget problem this public repository does not
  have, and prefers a plugin-root agent a project-local install cannot register.
  `agentic-actions-auditor` audits workflows that invoke AI coding agents and hard-stops when there are
  none; this repo's `.github/workflows` holds `ci.yml` and `release.yml` and invokes no such action, so
  every run would terminate at step 2 with no output. That one is excluded for having no plausible use
  today, explicitly not for quality.

### Held, not excluded

Two candidates are `HOLD`: real, non-duplicated capability, blocked by something specific. Neither is
installed, and neither becomes installed by anyone later deciding it "seems fine now" — an unblock is a
re-vendor through the update procedure below.

- **`insecure-defaults`.** *What it would add:* the most on-target excluded candidate in the whole slice.
  Its `references/fail-open-security.json` seeds include `InsecureSkipVerify[[:space:]]*:[[:space:]]*true`
  (Go/TLS) alongside fail-open environment-variable defaults, and its six categories — fallback secrets,
  default credentials, fail-open switches, weak crypto, permissive access, debug leakage — map onto this
  repo's `config.json` defaults, its `DASHBOARD_USERNAME`/`DASHBOARD_PASSWORD` basic auth, and its
  cookie/token handling. *What blocks it:* the plugin **ships no `SKILL.md` at all** — there is no skill
  directory to vendor. The entire mechanism is `commands/audit.md` invoking the `Workflow` tool with
  `name: "insecure-defaults:audit-pipeline"` and `pluginRoot: "${CLAUDE_PLUGIN_ROOT}"`, backed by a
  35,816-byte `workflows/audit.js` that fans out recon, six sweeps, refuting verifiers and a report.
  Nothing in that survives project-local skill vendoring. *What would unblock it:* either upstream ships
  the capability as a skill, or someone writes a first-party skill that uses the reference corpus (the
  `references/*.json` seed files are the durable, portable part) without the plugin workflow runtime.
  Vendoring `audit.js` and inventing a skill wrapper around it would be a rewrite, not minimal patching.
- **`second-opinion`.** *What it would add:* an independent review from a **non-Claude** model — a
  genuinely different capability that nothing else installed provides. Its Codex path is well constructed:
  `codex exec --sandbox read-only --ephemeral --output-schema codex-review-schema.json`, a structured JSON
  contract, headless. *What blocks it:* two things. First, it cannot run here — the skill's own "When NOT
  to Use" leads with "Neither Codex CLI nor Gemini CLI is installed", and neither `codex` nor `gemini` is
  on `PATH` in this environment, nor is any API key or subscription configured for either. Second, its
  Gemini path conflicts with this project's permission model: it invokes `gemini -p ... --yolo`, which the
  skill's own Safety Note describes as auto-approving all tool calls without confirmation. A tool that
  self-approves every action is the opposite of what `.claude/settings.json` and
  `.claude/hooks/governance-policy.py` exist to enforce. *What would unblock it:* install and pin the Codex
  CLI, provision its credentials outside the repository, and vendor the skill **with the Gemini path
  removed by patch** so no `--yolo` invocation is reachable — a patch narrow enough to stay minimal, but
  one that needs its own reviewed PR and a conscious decision that a second-model review is wanted at all.

## Invocation modes

All 23 installed skills are **model-invoked**, exactly as upstream ships them. No skill in this set is
renamed (`renamed_from: null` throughout) and none carries `disable-model-invocation`. Nothing here is
routed through a slash command by this project, and nothing here auto-runs: every one of these skills is
reached when the model judges the request matches its description, and the heavier ones gate themselves
(the `semgrep` skill's Step 3 is a hard approval gate before any scanner is spawned).

Frontmatter across the set is `name` + `description`, with two additions registered in the validator's
`extra_frontmatter_keys` for this provider:

- **`allowed-tools`** on 14 of the 23. This key *narrows* a skill's tool surface, so it is preserved rather
  than stripped. Three of the trailmark family write it as a YAML list (`- Bash`) rather than a space-
  separated string; both forms are upstream's and both are kept. One value was edited:
  `spec-to-code-compliance` dropped `Workflow` from its list, because a project-local skill install has no
  plugin Workflow runtime to grant (`tob-no-plugin-workflow`).
- **`type: technique`** on `harness-writing` — Trail of Bits' own skill-kind marker, meaningless to Claude
  Code and harmless, so it is kept verbatim rather than stripped to satisfy a key allowlist.

## License & attribution

**This is the section that differs most from the other five providers, because this is the only vendored
set that is not under a permissive licence.**

Upstream is licensed **CC BY-SA 4.0** (`SPDX: CC-BY-SA-4.0`), and upstream's own `README.md:148` states:
"This work is licensed under a Creative Commons Attribution-ShareAlike 4.0 International License. Made by
Trail of Bits." Creative Commons Attribution-ShareAlike is a copyleft licence with real, enumerated
conditions on redistribution, and vendoring 181 upstream files into this repository *is* redistribution.
Each condition and how this vendoring meets it:

- **§3(a)(1)(A) — retain attribution, the copyright notice, the licence notice, the disclaimer notice, and
  a URI or hyperlink to the Licensed Material.** The manifest's `license` block records
  `attribution: "Trail of Bits — https://github.com/trailofbits/skills"` and `spdx: "CC-BY-SA-4.0"`;
  `upstream_repo` and `upstream_commit` pin the exact material and give the link; every vendored skill
  directory carries the full licence text (below), which contains the licence notice and the §5 disclaimer
  of warranties verbatim; and upstream's `assets/trail-of-bits-mark.svg` is retained unmodified in 22 of
  the 23 directories. **One honest wrinkle, stated rather than glossed:** upstream's `LICENSE` file is the
  bare CC BY-SA 4.0 legal code with *no* copyright-holder line and no creator name in it. The
  identification of Trail of Bits as creator therefore comes from upstream's `README.md` — which is not
  vendored, being repo scaffolding rather than skill content — so this project carries that identification
  in the manifest's `license.attribution` field, in this document, and in the retained brand asset, rather
  than by inventing a copyright line into a licence file that upstream does not have one in.
- **§3(a)(1)(B) — indicate if You modified the Licensed Material and retain an indication of any previous
  modifications.** This is the obligation that the marker convention exists to satisfy. Every locally
  changed file carries `<!-- bukerov-local-patch: <id> -->` markers in the file itself, and every marker id
  resolves to a row in `docs/agents/trailofbits-skills-patches.md` stating what changed and why. Together
  those are this project's indication of modification. `provider-patch-marker-coverage` enforces the link
  in both directions — an in-file marker with no ledger row fails, and a manifest patch id with no ledger
  row fails — so the §3(a)(1)(B) notice cannot silently drift out of date.
- **§3(a)(2) — the notices may be satisfied in any reasonable manner, including by including the URI or
  hyperlink to a resource that includes the required information.** Rather than rely on a link, the full
  upstream `LICENSE` (blob `3b7b82d0da2db857eda1a798dbd908ea136f07b5`) is **copied verbatim into every one
  of the 23 vendored skill directories** as `.claude/skills/<name>/LICENSE`. Twenty-three byte-identical
  copies, each hash-verified in the manifest, each enforced by `provider-license-files`. A skill directory
  is the unit someone copies, reads or extracts, so the licence travels with it rather than sitting once at
  a root nobody looks at.
- **§3(b) — ShareAlike.** If You Share Adapted Material, You must license it under CC BY-SA 4.0 or a
  BY-SA-Compatible Licence, and You may not impose additional restrictions on it. Every local patch in this
  set is an **adaptation of BY-SA material and is therefore itself CC BY-SA 4.0** — not GPL-3.0, not
  unlicensed. That is the correct reading and it is stated here so nobody later assumes the repository's
  root licence swallowed them. It also means the patches must stay minimal for a licensing reason as well
  as a governance one: the smaller the adaptation, the less ambiguity there is about what is Trail of Bits'
  work and what is this project's.

**Relationship to this repository's own licence.** `bukerov-twitch-miner-go` is **GPL-3.0**. That licence
covers the miner's Go source — `cmd/`, `internal/`, the templates and assets it embeds — and it does
**not** cover this vendored documentation. The two are separately-licensed works that coexist in one
repository: a CC BY-SA 4.0 corpus of audit methodology under `.claude/skills/`, and a GPL-3.0 program
everywhere else. Nothing has been relicensed in either direction, and nothing here should be read as
relicensing anything.

As a standing compatibility fact, unconnected to any action taken here: Creative Commons designates
**GPLv3 as a one-way compatible licence for CC BY-SA 4.0** — an adaptation of BY-SA material may be
relicensed under GPLv3, and the reverse is not permitted. That is a fact about the licences, not a
description of this repository. **This project has performed no such relicensing**, and this vendored
documentation remains under CC BY-SA 4.0.

Two adjacent notices that do **not** cover this set: `.claude/skills/LICENSE` is the Matt Pocock set's
shared MIT notice, and the Apache-2.0 `LICENSE.txt` files under `.claude/skills/skill-creator-anthropic/`,
`frontend-design/` and `webapp-testing/` belong to the Anthropic set. Six providers, six independent
attributions.

## Local patches summary

**Eight patch ids across 27 touched files**, plus one file-exclusion recorded in the ledger without an id
(the `openai.yaml` deletion, which can carry no in-file marker). Twenty-four files have content changes;
three have a mode change only, with content byte-identical to upstream. By id — the counts sum to 29
rather than 27 because `codeql/SKILL.md` and `variant-analysis/SKILL.md` each carry two ids:

| Patch id | Files | What it is |
| --- | --- | --- |
| `tob-no-plugin-workflow` | 5 | Replaces pointers to plugin slash commands and the `Workflow` tool — which do not exist in a project-local install — with the equivalent explicit dispatch. |
| `tob-dead-pointer` | 6 | Removes or retargets references to skills that are not installed here (`issue-writer`, the `testing-handbook-skills` fuzzers, three `domain-specific-audits` corpora). |
| `tob-plugin-agents-relocated` | 4 | Drops the now-wrong `skills/<skill>/` segment from `{baseDir}` paths inside relocated agent files. Mechanical. |
| `tob-no-hidden-unicode` | 3 | Removes U+200B / U+200D characters that this repo forbids outright, preserving rendering by other means. |
| `tob-mode-normalize` | 3 | `100755` → `100644`. No byte of content changes. |
| `tob-exec-bit-interpreter` | 6 | Prefixes `bash` onto the six bare-path `.sh` invocations that the `100644` normalization above would otherwise make fail with `EACCES`. Concrete project incompatibility — patch ground (c). |
| `tob-no-tree-mutation` | 1 | Replaces a `git checkout <baseline>` that moves HEAD mid-review with a temporary worktree. |
| `tob-effort-pin-dropped` | 1 | Removes the `effort: low` frontmatter line the upstream rework added to `property-based-testing/SKILL.md`. |

Three of those deserve a sentence here rather than only in the ledger:

**`tob-effort-pin-dropped`.** The upstream rework pins `effort: low` in the skill's frontmatter. Claude
Code documents that key as "Effort level when this skill is active. **Overrides the session effort
level**" — upstream's own README states the same ("`effort` overrides the session level in both
directions, so this drags a deliberate `xhigh` session *down* while the skill is active"). The
**decisive ground for removal is authority**: the pin silently overrides an owner-selected session
effort whenever this model-invocable skill fires, and `effort` is on this repository's recorded
authority-surface key list (`docs/agents/skills-update-automation.md`). A mechanical constraint sits
alongside, stated precisely because the two must not be conflated: this provider's
`extra_frontmatter_keys` allowlist in `scripts/validate-agent-governance.py` is `{allowed-tools,
type}`, so the key as vendored fails `check_frontmatter_keys` — but the validator's own documented
default for a *legitimate* upstream key is the opposite of deletion: "PRESERVE that frontmatter and
widen the allowlist per provider" (`allowed_frontmatter_keys_by_dir`, whose docstring even names
`effort` as an example). That widening route exists and was **deliberately declined, not overlooked**:
it is a reviewed validator change no re-vendor task performs as a side effect, and the authority ground
above makes adopting the pin undesirable here regardless of the allowlist. The honest cost: upstream
measured `low` as sufficient on its own eval fixture (4/4 defect detection) and chose it to cut cost;
removing the pin forgoes that optimization and the skill inherits the session's effort instead — which
is precisely the pre-rework behaviour. If the pin is ever wanted, the route is that reviewed allowlist
widening plus deliberately re-adding the line, not silently keeping upstream's default. No other byte
of the reworked skill is modified; one knock-on is recorded in "Known limitations" (the vendored README
still describes the pin as present).

**`tob-no-hidden-unicode`.** Upstream used **U+200B ZERO WIDTH SPACE** in two `ANALYSIS_FORMAT.md` files to
stop a nested code fence from closing its outer block — an invisible character placed immediately before
each inner ` ``` `. This repository forbids invisible and bidirectional characters outright as a security
invariant (`HIDDEN_UNICODE` in `scripts/validate-agent-governance.py`: U+200B–U+200F, U+FEFF, U+202A–U+202E,
U+2066–U+2069), because a character you cannot see is a character you cannot review. The zero-width spaces
were removed and the **outer fence widened to four backticks** — standard CommonMark for nesting a fenced
block — which renders identically with no invisible character. The third instance is different in kind: a
Swift family-emoji literal in `sharp-edges/references/lang-swift.md` whose three **U+200D ZERO WIDTH
JOINER** characters are the *subject* of the example, not formatting. They are now spelled `\u{200D}`,
which is valid Swift and shows the reader exactly what the literal contains — arguably clearer than the
raw joiners were.

**`tob-no-tree-mutation`.** `differential-review/methodology.md` told the agent to `git checkout
<baseline_commit>`, analyse, then check back out to head. That moves HEAD and mutates the working tree of
whatever repository the review is running in, mid-review, with the agent's own uncommitted state at risk.
It is now `git worktree add /tmp/baseline-review <baseline_commit>`, with a matching `git worktree remove`
where the checkout-back-to-head line was. Same baseline, same analysis, no tree mutation.

Full ledger, one row per patch id per touched file: `docs/agents/trailofbits-skills-patches.md`.

### Default: minimal patching

Under Governance v3 (`docs/adr/0002-governance-v3-skill-native-orchestration.md`), skills are preserved as
close to their authors' intent as practical. **Do not patch a skill merely because it uses subagents,
several writers, reviewers/critics, parallel analysis, iterative fixes, or its own handoff/orchestration
pattern** — including fan-out that upstream leaves unbounded. That is engineering workflow, and workflow
belongs to the skill (see `docs/agents/agent-orchestration.md`). Patch only for concrete project
incompatibility, a broken dependency, license/provenance necessity, or a genuine authority/integrity
boundary.

This provider is the strongest test of that rule the repository has, because its whole value is
orchestration. **Every subagent topology in this set was preserved.** Concretely, and none of these was
capped, serialized or made read-only by this vendoring:

- `fp-check` fans out to three distinct subagents — `data-flow-analyzer`, `exploitability-verifier`,
  `poc-builder` — and its three agent files are byte-identical to upstream.
- `audit-context-building` dispatches **one subagent per function** over an entire codebase, unbounded, and
  has each write its own prose to disk while returning only a compact record. Untouched.
- `spec-to-code-compliance` dispatches one checker per requirement and then a *second, different* agent to
  refute each divergence before it may be reported. That refutation lane is the skill's central claim about
  why it works, and it is intact; the `tob-no-plugin-workflow` patch replaced the missing slash command
  with the equivalent explicit Task dispatch, deliberately preserving the same fan-out, the same
  independent-refuter requirement, and the same structured-record contract rather than narrowing any of
  them.
- `variant-analysis` fans the five steps over parallel subagents, one per expansion axis, looping until the
  sweep stops finding anything new. The patch retitled the section from "Running it as a Workflow" to
  "Running it in Parallel" and pointed it at Task; the loop and the fan-out are unchanged.
- `differential-review` delegates to `adversarial-modeler`, and `sharp-edges` to `sharp-edges-analyzer`,
  both preserved — indeed both were *rescued* by the relocation adaptation rather than dropped.

No patch in this set caps concurrency, imposes a writer count, forces any reviewer into a read-only role,
or reorders a repair loop. The seven ids are, without exception, about a missing plugin runtime, a pointer
to something that is not installed, a path segment, an invisible character, a file mode, an interpreter
prefix that file mode made necessary, and a git command that moves HEAD.

## Governance precedence

Vendored skills sit **below** this project's own policy **on authority**. The authority chain has exactly
four levels (see `CLAUDE.md`'s "Claude Code Governance (v3)" section and
`docs/agents/agent-orchestration.md`), narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched, this set alongside every other.
4. **Generic model behavior** — fallback only.

Unpatched upstream text is **not** a further tier below the patches: a vendored skill's instructions are
whatever its vendored bytes say, patched and unpatched alike, all together at level 3. Where a local patch
and the upstream text around it disagree, the patch wins — that is what patching means, and it is resolved
inside level 3. A skill instruction never overrides a `.claude/rules/*.md` constraint or a hook denial.

**On workflow the order is inverted**: an invoked audited skill owns its documented engineering
methodology — agents, lanes, reviewers, writers, repair loops — and the project does not override it. See
`docs/agents/agent-orchestration.md`, and the preserved topologies listed above.

The non-delegable prohibitions bind every agent at every delegation depth, whatever a skill's own text
says: no marking a PR ready for review, no merge or auto-merge (**the owner performs merges**), no
release, tag or deploy, no triggering or rerunning a GitHub Actions workflow, no GitHub settings or secrets
changes, no force push, no direct push to `main`/`master`.

Three of this provider's skills sit close enough to that boundary to be worth naming. `differential-review`
and `trailmark-review-gate` are both PR-shaped: they review a branch, a PR or a ref range and produce a
verdict. A verdict is a report. Neither may merge, mark ready for review, or dispatch CI to confirm
anything, however conclusive its own output reads — and `differential-review`'s "Address CRITICAL/HIGH
issues before merge" line is advice to the owner, not an authorization to act. `semgrep` and `codeql` are
read-only over their target by construction (no `--autofix`, every write inside the output directory), and
`semgrep`'s approval gate is a scope confirmation on top of that, not the safety mechanism.

## Supply-chain assumptions

Same rationale as the other five providers: a live plugin install trusts upstream's default branch on every
future run, not just the commit reviewed today. Vendoring converts that into "trust as of a specific
reviewed SHA, re-established only when someone deliberately re-reviews."

This provider's manifest uses the **file-level** schema: every `files[]` entry carries both an
`upstream_blob_sha` (what upstream had at the pin) and a `vendored_blob_sha` (`git hash-object` of the file
as committed here). `provider-file-hashes` fails closed on any on-disk edit to any vendored file — patched,
unpatched, or local-origin — that isn't accompanied by a deliberate `vendored_blob_sha` bump in the
manifest. That is the re-audit forcing function, and it matters more here than for a Markdown-only
provider: this set ships 29 executable-content files (25 Python, 4 shell), so "does the script still say
what the audit said it said" is a live question rather than a formality. With `GOVERNANCE_UPSTREAM_DIR_TRAILOFBITS` pointing at a
read-only clone at the pin, `upstream_blob_sha` is additionally verified against upstream itself, so
divergence *from upstream* is detected rather than mere self-consistency.

**External scanners are a runtime capability, not an install prerequisite, and none was installed by this
task.** This is the single most important thing to understand about what was and was not done here.
Installing the `semgrep` skill installs *instructions for running Semgrep*; it does not install Semgrep.
Likewise `codeql` does not install the CodeQL CLI, and the trailmark family does not install the
`trailmark` package. Verified directly at review time: `semgrep`, `codeql`, `trailmark`, `codex` and
`gemini` are all absent from `PATH` in this environment; `uv` is present. **No scan of any kind was run
against this repository as part of this vendoring** — not Semgrep, not CodeQL, not mutation testing, not a
Trailmark graph build. Nothing in this document or the manifest should be read as reporting a scan result.

The residual supply-chain surface is therefore what these skills *would reach for* if someone ran them, and
it was reviewed on that basis:

- **`semgrep/scripts/run-scans.sh` clones third-party rule repositories at runtime** —
  `git clone --depth 1 "$url"`, default branch, unpinned — from the twelve sources catalogued in
  `references/rulesets.md`: `trailofbits/semgrep-rules` (AGPLv3), `0xdea`, `Decurity`, `dgryski`,
  `mindedsecurity`, `elttam`, `kondukto-io`, `federicodotta`, `hashicorp-forge`, `akabe1`,
  `atlassian-labs`, `apiiro`. Those clones happen only after the skill's Step 3 approval gate, and the
  rules are read by Semgrep rather than executed as programs — but "approved the scan" and "reviewed a
  dozen third-party rule repositories at their current default branch" are not the same act, and the
  approving human should know which one they are performing.
- **`trailmark/SKILL.md` instructs `uv pip install trailmark` if the command is missing**, unpinned, from
  PyPI, and explicitly forbids falling back to manual analysis. That is upstream's text and it was left
  unpatched — it is a runtime dependency decision, not an authority boundary — but installing a package
  from PyPI on the strength of a skill instruction is a decision for whoever runs it, and this policy does
  not pre-authorize it.
- **`genotoxic` instructs installing a toolchain too, and it is the one whose installs land in *this*
  repo's toolchain.** Its Prerequisites repeat the `uv pip install trailmark` instruction above with the
  same "**DO NOT** fall back to manual analysis" wording, extend it to "install it using the instructions
  in `references/mutation-frameworks.md`" for whichever mutation framework the target language needs, and
  recommend `cargo install necessist`. For Go, `references/mutation-frameworks.md` says
  `go install github.com/go-gremlins/gremlins/cmd/gremlins@latest` (or `brew tap go-gremlins/tap && brew
  install gremlins`) and `go install github.com/zimmski/go-mutesting/cmd/go-mutesting@latest`; the same
  file carries `cargo install cargo-mutants`, `gem install mutant`, `uv tool install slither-analyzer` and
  `apt-get`/`brew` installs of LLVM and Mull for the other languages. All unpinned, all `@latest` or
  equivalent. None of it is patched — it is upstream's own runtime dependency guidance — but a `go install
  …@latest` puts an unpinned third-party binary on the invoker's `PATH`, and this policy pre-authorizes
  none of them.
- **`codeql` and `supply-chain-risk-auditor` reach the network too** — query packs and advisory databases
  respectively. Same reasoning.
- Several *excluded* candidates were rejected partly on this axis, which is why they were read in full
  rather than skimmed: `ossfuzz` (enrollment in a Google-hosted service), `second-opinion` (external model
  CLIs plus a `--yolo` auto-approve path), `slicing-code-context` (a 1105-line script pinned to
  Trailmark 0.5.x for a capability with no consumer here).

What was *not* done, stated plainly: no test suite was executed, no static-analysis scan was run, and no
vendored script was executed dynamically. The review consisted of reading the candidate skills and their
complete dependency closures, reading every bundled script in the six script-bearing installed skills end
to end, diffing the vendored tree against a read-only clone at the pin, and recording hashes.

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
   `GOVERNANCE_UPSTREAM_DIR_TRAILOFBITS` to that clone's path for the validator's stricter blob-hash mode.
2. Diff the set of directories under `plugins/*/skills/` against the last-reviewed list — additions,
   removals and renames among the 23 installed skills, **and** whether any of the 25 recorded candidates
   changed in a way that alters its verdict (especially the two `HOLD` entries: has upstream given
   `insecure-defaults` a `SKILL.md`? has `second-opinion`'s Gemini `--yolo` path changed?).
3. **Re-scope, don't assume.** This review covered a slice, not the corpus: 78 skill directories exist and
   31 were never itemised because their plugin's subject matter placed them out of scope. Re-check that
   judgement against the current pin — a new plugin, or new skills inside an existing one, can land in
   scope. Record every new candidate in `excluded_skills[]` with a reason and an `EXCLUDE`/`HOLD` verdict,
   even the easy nos; the value of that list is that the next reviewer doesn't repeat the work.
4. **Re-check the two structural adaptations, both of which are silent failure modes.** (a) Does each
   installed skill's upstream `agents/` directory still contain only `openai.yaml`? If upstream starts
   shipping a real agent definition there, dropping the directory would now drop a dependency. (b) Does
   each of the five dispatching skills still dispatch exactly the plugin-root agents relocated here, and
   have any new ones appeared at `plugins/<plugin>/agents/`?
5. For each vendored skill, diff every file in its `files[]` list against the **currently-vendored copy**
   (not raw upstream — 24 files carry content patches, and three more differ from upstream only in mode) to
   separate genuinely new upstream content from an existing patch.
6. Re-run the same review judgment on anything new: does it assume a plugin runtime, background execution,
   external network fetch, credentials, or a write into a git working tree this project doesn't grant? Does
   it introduce an invisible Unicode character or an executable bit? If so, patch it minimally and mark it
   — and re-confirm `scripts_audited: true` by reading any touched script end to end. The test is
   **authority**, not orchestration: a skill's agent topology, fan-out width, writer count, reviewer lanes
   and repair loops are never grounds for a patch (see "Default: minimal patching").
7. Update `upstream_commit`, `upstream_tree`, `upstream_current_head`, `drift` and `reviewed_at` in
   `trailofbits-skills-manifest.json`; update every touched file's `upstream_blob_sha`/`upstream_mode` and
   `locally_modified`/`patch_ids`. If upstream's root `LICENSE` blob changed, re-copy it into all 23 skill
   directories and update all 23 entries — a CC BY-SA obligation, not a formality. Recompute
   `vendored_blob_sha` (`git hash-object <path>`) for every touched file **last**, after all edits for the
   round are finalized; that is what re-pins the file and clears the fail-closed check.
8. Update `trailofbits-skills-patches.md` for any patch added, changed or removed, and this policy's
   "Installed" / "Excluded / Held" / "Local patches summary" sections if the set or the patches changed.
9. Run `python3 scripts/validate-agent-governance.py` and fix every reported failure.
10. Open the change as its own dedicated Draft PR (see `mattpocock-skills-policy.md`'s "Dedicated Draft PR
    requirement" — the same rule applies here); never bundle a skills re-vendor into an unrelated change.
    Get human review before merge; this policy forbids the agent from merging it.

## Rollback

1. Identify the last-known-good `upstream_commit` from `trailofbits-skills-manifest.json`'s git history.
2. Restore the affected `.claude/skills/<name>/` directories from that commit with
   `git checkout <sha> -- <paths>`. The 23 directory names are in the manifest's `skills[].path`; restoring
   the whole set is safer than picking a subset, because several skills cross-reference each other
   (`genotoxic` → `harness-writing`, `variant-analysis` → `audit-context-building`, the trailmark family
   → `trailmark`).
3. Restore `docs/agents/trailofbits-skills-manifest.json` and `trailofbits-skills-patches.md` from the same
   commit (and this policy, if it moved with them).
4. Run `python3 scripts/validate-agent-governance.py` to confirm consistency.
5. Open a dedicated PR for the rollback with the reason in the description.

Rolling back this provider is independent of the other five: no other manifest claims any of these 23
directories, and no patch id in this set is shared with another provider's ledger.

## Known limitations

- **No scanner is installed, and none was run.** Restated here because it is the likeliest thing to be
  misread from the presence of `semgrep`, `codeql` and `trailmark` in the skill list. Installing the skill
  installs the methodology, not the tool. Anyone invoking one of these skills on a machine without the
  corresponding CLI will get an install instruction, not an analysis.
- **Three shell scripts were invoked by bare path and carry no executable bit — now patched, not
  documented around.** `codeql/SKILL.md:107` and two of its workflow documents called
  `"{baseDir}/scripts/find_databases.sh" ...` directly, `references/run-all-suite.md` and
  `references/important-only-suite.md` called `{baseDir}/scripts/generate_suite.sh <mode>`, and
  `semgrep/workflows/scan-workflow.md:263` called `{baseDir}/scripts/run-scans.sh`. Under the `100644`
  mode normalization every one of those fails with `EACCES`. An earlier draft of this policy left them
  unpatched and told the reader to substitute `bash <path>` by hand; that was the wrong call. A skill
  that fails on first use is a **concrete project incompatibility** — patch ground (c) — and six `bash `
  prefixes are a far smaller cost than a footnote every future invoker has to remember. All six call
  sites now read `bash "{baseDir}/scripts/<name>.sh"`, under `tob-exec-bit-interpreter`. Semantics are
  unchanged, and the mode invariant (`no-symlinks-no-exec-under-claude`) stays absolute.
  Note the scope: `uv run {baseDir}/scripts/*.py` invocations elsewhere in the set were **not** touched —
  `uv run` is already an explicit interpreter and needs no executable bit.
- **`{baseDir}` is not substituted by anything.** No loader expands it; the validator resolves it for link
  checking only. An agent reading a vendored file must understand `{baseDir}` as "the directory this skill
  lives in" and construct the real path itself. That is upstream's own convention and this vendoring did
  not change it.
- **The relocated agent files are not registered Claude Code subagents.** They sit at
  `.claude/skills/<skill>/agents/*.md`, which is inside the skill's own directory; this repository has no
  `.claude/agents/` directory and defines no agents there. So an instruction like "dispatch the
  `spec-to-code-compliance:spec-compliance-checker` agent" will not resolve a registered agent by that
  name. What the relocation preserves is the **definition** — the file is present, complete and reachable,
  so the orchestrating skill can pass its content into a `Task` subagent prompt. That is the difference
  between the topology being documented and it being auto-wired, and only the former is true here.
- **`excluded_skills[]` itemises 25 candidates, not all 55 non-installed skills.** Twenty-four of the 25
  are among those 55 (the twenty-fifth, `insecure-defaults`, is a plugin with no skill directory); the
  other 31 carry no per-candidate verdict, having been ruled out at plugin granularity by subject matter
  with no per-skill read (listed under "Excluded / Held"). That gap is not hypothetical:
  `audit-prep-assistant`, inside the smart-contract plugin `building-secure-contracts`, runs
  `golangci-lint` on Go codebases and is not itself out of scope. Step 3 of the update procedure exists
  for that reason.
- **`differential-review/patterns.md` still points at `building-secure-contracts/development-guidelines`.**
  The `tob-dead-pointer` patch removed the bullets naming `not-so-smart-contracts`,
  `token-integration-analyzer` and the three `domain-specific-audits` corpora, but left this one: it names
  an upstream *plugin* (which exists at the pin, just uninstalled here) rather than a skill this install
  claims to provide. Reaching it means going to upstream, which is a legitimate thing for a reader to do.
  Flagged so nobody reads the surviving bullet as an installed dependency.
- **`property-based-testing/README.md` describes upstream state this install does not have.** Vendored
  verbatim at the current pin, it documents the `effort: low` frontmatter pin as present in SKILL.md —
  including the note that `effectiveness.sh` refuses a multi-level sweep "while `SKILL.md` pins an
  effort" — while `tob-effort-pin-dropped` removes that pin from the vendored SKILL.md; and its eval
  walkthrough invokes `evals-extra/run.sh` / `evals-extra/effectiveness.sh`, which live at upstream's
  plugin root and are not vendored, so every command it prints is unrunnable here. Nothing dispatches
  through the README (SKILL.md routes only to `references/*.md`), so this is inert documentation of
  upstream's own eval machinery, kept verbatim on the same minimal-patching ground as the other
  surviving upstream-layout references. Flagged so nobody reads it as a description of the vendored
  copy.
- **Most of this set is scoped to languages and platforms this repository does not use.** `sharp-edges`
  ships eleven per-language footgun guides of which one is Go; `variant-analysis` ships five CodeQL and
  five Semgrep templates of which one each is Go; `codeql` documents eight languages. That breadth is why
  the fuzzing and smart-contract skills were excluded rather than installed "just in case", and it is worth
  knowing when reading a skill's output: material about Solidity or C++ in a Go review is upstream breadth,
  not a finding.
- **The hook/permission layer is a backstop, not a substitute for reading a skill.** As
  `mattpocock-skills-policy.md`'s "Known limitations" section notes, a sufficiently subtle instruction can
  still shape agent *reasoning* even where it cannot force a blocked tool call. That caveat applies to this
  set identically — and applies with more force, since this is the largest vendored set and its skills are
  the ones most likely to be reasoning about security-sensitive code — and is not repeated in detail here.

## Compatibility

Reviewed and vendored against the same Claude Code version and governance-layer assumptions recorded in
`mattpocock-skills-policy.md`'s "Compatibility" section (frontmatter, permission and hook-payload
behavior) — see that document rather than duplicating the version pin here; re-verify them together if
either changes.

Three provider-specific notes. First, this upstream targets **two** agent platforms: the vendored bodies
are written for Claude Code, but 22 of the 23 also shipped Codex marketplace metadata
(`agents/openai.yaml`, dropped) and upstream's `AGENTS.md` addresses both. A future upstream may introduce further
platform-specific files inside skill directories; the `forbidden-vendor-files-absent` check catches the
known names, and anything new fails the frontmatter or manifest checks rather than being silently
accepted. Second, this provider's `extra_frontmatter_keys` allowlist is `{allowed-tools, type}` — widening
it is a reviewed decision, not a fix. Third, the whole set is written against a **plugin** runtime that
this install does not provide: `Workflow`, `${CLAUDE_PLUGIN_ROOT}` and `/<plugin>:<command>` slash commands
all appear in upstream text. The five files where that mattered are patched
(`tob-no-plugin-workflow`); if a future upstream leans harder on that runtime, the right response may be to
stop vendoring the affected skill rather than to grow the patch set.
