# Project-owned first-party skills — policy

## Purpose and ownership

This is the third skill ownership class in this repository, alongside the two vendored sets
(`docs/agents/mattpocock-skills-policy.md`, `docs/agents/anthropic-skills-policy.md`). A **project-first-party**
skill is authored and owned by this project itself: no upstream repository, no external maintainer, no vendored
copy of someone else's work. Its inventory lives in `docs/agents/project-skills-manifest.json`
(`ownership_class: "project-first-party"`), validated by `scripts/validate-agent-governance.py`'s
`project-manifest-valid` check. The manifest ships **empty** (`"skills": []`) until a first skill is reviewed and
installed through its own dedicated PR — this document and the validator extension are the governance foundation,
not an installation.

## Distinction from vendored upstream skills

Vendored skills (Matt Pocock's, Anthropic's) are copies of a third party's work, pinned to a reviewed upstream
commit, re-synced only by a deliberate re-vendor. A first-party skill has none of that: there is no upstream repo
to diff against, no upstream commit to pin, and no re-vendor procedure. Its lifecycle is exactly the same as any
other reviewed change to this repository — write it, review it, merge it, and update it in place. Any manifest key
containing `upstream` anywhere in a project entry is a schema violation (enforced by `project-manifest-valid`):
a first-party entry must never assert provenance it doesn't have.

## Authority precedence

Nothing here changes `CLAUDE.md`'s authority chain, which has exactly four levels (see
`docs/agents/agent-orchestration.md`), narrowing only — each layer may restrict, never widen:

1. **Owner / task contract** — the authority envelope.
2. **`CLAUDE.md` + `.claude/rules/*.md`** — repository safety and integrity invariants.
3. **Invoked audited skill instructions** — vendored skills as patched and this project's own first-party
   skills, at the same tier; ownership class changes review procedure, never authority.
4. **Generic model behavior** — fallback only.

There is no fifth tier for "unpatched upstream defaults": a first-party skill has no upstream at all, and a
vendored skill's instructions are whatever its vendored bytes say, patched and unpatched alike, resolved inside
level 3. A first-party skill instruction never overrides a `.claude/rules/*.md` constraint or a hook denial,
exactly like a vendored one.

On **workflow**, a first-party skill owns its documented methodology the same way a vendored one does (see
`docs/agents/agent-orchestration.md`) — the authority chain above constrains what it may reach, not how it
organizes its agents.

## Allowed invocation modes

Two enum values only, mirrored between the manifest entry's `invocation` field and the skill's own `SKILL.md`
frontmatter, and cross-checked by the validator:

- `model+user` — the skill may trigger automatically; its frontmatter must **not** carry
  `disable-model-invocation`.
- `user-only` — the skill only runs on explicit invocation (e.g. `/skill-name`); its frontmatter **must** carry
  `disable-model-invocation`.

The validator also checks the frontmatter's own `name:` value against the manifest's `name` field — they must
match exactly.

## Read-only vs mutation-capable classification

Every entry declares `mutation_capability`: `read-only` or `mutation-capable`. **This is metadata and review
evidence, not a capability boundary.** A `read-only` classification is a deterministic proxy the validator can
check (it requires `scripts: []` and `hooks: []` — no scripts, no hooks), and it records what the reviewer
concluded when the skill was last read end-to-end. It grants nothing by itself. Actual mutation authority — the
ability to edit, commit, push, or touch the tracker — comes only from the operation modes
(`docs/agents/operation-modes.md`) and an active task contract (`docs/agents/task-contract.md`), the same as for
every other skill, vendored or not. A skill marked `mutation-capable` in the manifest still cannot commit or push
outside an active contract that grants it; a skill marked `read-only` is not itself what stops a determined
override — the contract and hook layer are.

## Evaluation requirement before installation

No first-party skill installs without recorded evaluation evidence: real baseline-versus-candidate results,
produced the same way the vendored `skill-creator` tooling structures an eval (isolated evaluator runs plus an
independent comparator judging the outputs), not a self-report from the same session that wrote the skill.
`eval_evidence` in the manifest is a required object (`{path, blob_sha}`) pointing at that evidence.

**Durable evidence only.** `eval_evidence.path` must be a repository-relative path to a file that is:

- tracked by git (`git ls-files` must show it — not merely present on disk),
- pinned by content (`blob_sha`, checked against `git hash-object`),
- **not** under `.scratch/` or `/tmp` (those are ephemeral working areas, not durable records),
- **not** a mutable PR description, a chat transcript, or an unpinned external URL — none of those survive a
  rebase, an edit, or a link rotting, and none of them are checkable by a deterministic validator.

## Independent review requirement

Every first-party skill install or update goes through the same two-lens review this repository uses elsewhere
(`code-review` skill): a **Standards** review (does it follow this repo's conventions, minimal footprint, no
unjustified scripts/hooks) and a **Spec** review (does it do what it claims, does its eval evidence actually
support the claim). The agent that writes the skill is never the agent that approves it — reviewer
independence is a review-integrity requirement for this specific category of change, not a general
orchestration rule (see `docs/agents/agent-orchestration.md`, which leaves reviewer topology to the invoked
skill).

## Manifest integrity rules

Enforced mechanically by `project-manifest-valid` (see `scripts/validate-agent-governance.py`):

- Every file under an entry's skill directory must appear in that entry's `files[]` — a directory walk, same
  shape as the anthropic vendored check, skipping only `__pycache__`/`*.pyc`. **A symlinked subdirectory is
  itself a violation and is never descended into** — the walk flags it directly (it can't otherwise see, and
  therefore can't otherwise police, whatever content sits behind it), so a first-party skill directory must
  contain only real files and real directories, never a symlink to one.
- Every `files[]` entry pins `path` (must live exactly under the entry's own skill directory, no traversal, no
  glob metacharacters), `blob_sha` (`git hash-object`, 40-hex), and `mode` (must be `"100644"` — never
  executable). A rejected `path` (e.g. one that fails these shape rules) is never used to satisfy any other
  check's membership test — only a shape-valid path counts as "listed."
- `SKILL.md` must exist and be listed.
- Any declared file must exist on disk, must not be a symlink, and must carry no executable bit.
- `name` must match `^[a-z0-9][a-z0-9-]*$`; `path` must equal exactly `.claude/skills/<name>`. An entry whose
  `path` fails shape validation (traversal, absolute) is never used to build a filesystem path at all — no
  directory-existence check and no directory walk are attempted for it; the shape diagnostic is the only
  diagnostic the validator ever emits for that entry's directory.
- Two entries must never share a `name` or a `path`, even if both are individually well-formed — a duplicate
  is a violation in its own right, detected both within a single manifest and across all three ownership
  sources (`unique-skill-names`, `manifest-ownership-partition`).
- `review_status: "approved"` is required once an entry's skill directory exists on disk. `"draft"` is
  schema-valid only for a staged entry with no on-disk directory yet — the moment content lands, review must
  have happened.

### Worked example entry

The manifest ships empty today (see "Purpose and ownership" above); this is the full shape a first accepted
entry must take — every required key, so following this document alone is enough to write a manifest the
validator accepts:

```json
{
  "schema_version": 1,
  "ownership_class": "project-first-party",
  "notes": "First-party, project-owned Claude Code skills authored and reviewed in this repository.",
  "skills": [
    {
      "name": "example-skill",
      "path": ".claude/skills/example-skill",
      "origin": "project",
      "invocation": "model+user",
      "mutation_capability": "read-only",
      "scripts": [],
      "hooks": [],
      "files": [
        {
          "path": ".claude/skills/example-skill/SKILL.md",
          "blob_sha": "000000000000000000000000000000000000000a",
          "mode": "100644"
        }
      ],
      "eval_evidence": {
        "path": "docs/agents/eval-evidence/example-skill.md",
        "blob_sha": "000000000000000000000000000000000000000b"
      },
      "review_status": "approved",
      "reviewed_base_sha": "000000000000000000000000000000000000000c",
      "reviewed_at": "2026-07-27"
    }
  ]
}
```

Notes on the fields above that aren't self-explanatory: `schema_version` is always the integer `1`;
`ownership_class` is always the literal string `"project-first-party"` (checked at the manifest's top level, not
per entry); `origin` is always the literal string `"project"` — any key containing `upstream` anywhere in an
entry is rejected outright (see "Distinction from vendored upstream skills"); `blob_sha` values are real
`git hash-object` output (40 hex characters) — the placeholders above are shape examples only, never usable
values; `eval_evidence.path` must be a tracked, repository-relative file (see "Evaluation requirement before
installation"); `reviewed_base_sha` is the 40-hex `main` SHA the approving review ran against, and `reviewed_at`
is that review's `YYYY-MM-DD` date.

## Scripts and hooks policy

**None by default.** A first-party skill starts prose-only. Adding a script or a hook requires, in the same PR:

- a demonstrated failure mode the prose-only form can't cover (not "it'd be convenient"),
- a measurable benefit over the prose-only alternative,
- an explicit, exact declaration in `scripts[]`/`hooks[]` (repository-relative paths, no globs, and each entry
  must live under the entry's own skill directory) — every declared script/hook must also appear in `files[]`.

The validator enforces this bidirectionally and by exact location, not just by name:

- every on-disk file that is script-shaped (`.py`/`.sh`/`.js`/`.mjs`/`.rb`/`.pl`/`.ts`/`.bash`/`.zsh`/`.ps1`, or
  anywhere under the skill's `scripts/` subdirectory, or any file at all whose first two bytes are `#!`) must be
  declared in `scripts[]`;
- every on-disk file under the skill's `hooks/` subdirectory must be declared in `hooks[]`;
- every `hooks[]` entry must itself live under `<skill>/hooks/` — a declared hook anywhere else is rejected, so
  "declared as a hook" and "physically located where a hook lives" can never drift apart.

Drift in any of these directions — undeclared on-disk script/hook, declared-but-missing, or a hook declared
outside `hooks/` — fails the validator.

Hooks carry a stricter bar than scripts: a hook runs on every matching tool call for every session that loads it,
so its justification must be the demonstrated failure mode specifically, not general usefulness.

No hidden persistent state: a first-party skill must not read or write files outside its own directory except
through the ordinary tool surface an agent already has, and it must not stash state between sessions. Temporary
artifacts it produces while running belong under `/tmp` or `.scratch/` only — never inside `.claude/skills/**`
(which stays a purely reviewed, versioned tree) and never used as `eval_evidence` (see above).

## Dedicated Draft PR requirement

Each skill installation or update lands in its own dedicated Draft PR — same rule as the two vendored sets (see
`mattpocock-skills-policy.md`'s "Dedicated Draft PR requirement"). Never bundle a first-party skill change into
an unrelated feature or governance diff.

## Update procedure

An update is an ordinary reviewed edit, not a re-vendor: edit the skill content, bump every changed file's
`blob_sha` (and `eval_evidence.blob_sha` if the evidence file changed) in the same PR, re-run
`python3 scripts/validate-agent-governance.py` until clean, and get the same Standards + Spec review as a new
install. `reviewed_base_sha`/`reviewed_at` are updated to the SHA and date of the review that approved the
change.

## Rollback

Revert the install (or update) PR. The manifest entry and the skill's directory always move together — reverting
one without the other would leave either a phantom manifest entry or an unclaimed directory, both of which
`project-manifest-valid` and `manifest-ownership-partition` fail closed on.

## Two-PR bootstrap model

This governance foundation (this document, the empty manifest, the validator extension, the
`.claude/rules/github-governance.md` update) lands first, on its own, policing zero project skills. Any first
skill installation is a separate, later PR. The enforcement layer must exist and be reviewed before the first
content it polices exists — a validator that ships in the same diff as the skill it's meant to check can never
prove it would have caught a problem in that skill.

## Human-only merge; no automatic updates

Same as both vendored policies: an agent never merges its own work here (see `CLAUDE.md`'s non-delegable
prohibitions). There is no CI job, scheduled task, or plugin mechanism that re-syncs or auto-updates a first-party
skill — every change is a deliberate, human-reviewed PR.
