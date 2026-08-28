"""Deterministic rendering of analyses into text, JSON, PR bodies and issue bodies.

Every function here is a pure function of its inputs. In particular **nothing in this module
reads the clock**. That is a hard requirement rather than a stylistic one: issue bodies are
compared against the previous run's body to decide whether an existing blocked-update issue
needs updating, and a timestamp in the body would make every run look like a change and
re-notify every subscriber daily. Run-specific context (run id, run URL) belongs in the job
summary, which nothing compares.

The other invariant is that no upstream-controlled string is ever interpolated into a position
where it could be read as markup or as a command. Paths, skill names and blocked-reason details
originate upstream; they are rendered inside fenced blocks or as plain list text, never as a
Markdown link target and never into a shell string.
"""

import json

from . import states
from .candidate import PR_BANNER

#: Fence used for any block containing upstream-controlled text. Four backticks, so a payload
#: containing a normal three-backtick fence cannot close ours early and escape into markup.
FENCE = "````"


def _fenced(lines):
    """Render `lines` inside a fence that upstream content cannot break out of.

    The fence alone is not enough, and assuming it was is how upstream text escapes into markup.
    Blocked-reason details carry raw upstream paths, and `ls-tree -z` deliberately preserves a
    path containing a newline or a backtick -- so a crafted path could close the fence and
    continue as Markdown. Each line is neutralized first: any run of backticks is replaced, and
    embedded newlines are folded, so a single detail stays a single line inside the fence.
    """
    safe = []
    for line in lines or []:
        text = str(line).replace("\r", " ").replace("\n", " ⏎ ")
        safe.append(text.replace("`", "'"))
    return "%s\ntext\n%s\n%s" % (FENCE, "\n".join(safe) if safe else "(none)", FENCE)


def _identity_document(identity):
    if identity is None:
        return None
    if isinstance(identity, dict):
        return dict(identity)
    method = getattr(identity, "to_dict", None)
    if not callable(method):
        raise TypeError("stable identity must be a dict or CandidateIdentity")
    return method()


def to_json(analyses, plugin_drifts=None, stable_identity=None):
    """Machine-readable form of a whole run. Key order is fixed; providers keep config order."""
    payload = {
        "schema": "skills-update-check/2",
        "g1_1_mode": "artifact-only",
        "publication_authority": "UNCOMMISSIONED",
        "stable_identity": _identity_document(stable_identity),
        "providers": [a.to_dict() for a in analyses],
        "plugins": [d.to_dict() for d in (plugin_drifts or [])],
        "summary": {
            "checked": len(analyses),
            "drifted": sum(1 for a in analyses if a.drifted),
            "blocked": sum(1 for a in analyses if a.is_blocked),
            "prepared_audit_required": sum(
                1 for a in analyses if a.state == states.PREPARED_AUDIT_REQUIRED),
            "discovery_required": sum(1 for a in analyses if a.discoveries),
            "eval_required": sum(1 for a in analyses if a.eval_required),
            "plugin_drifts": len(plugin_drifts or []),
            "states": {state: sum(1 for a in analyses if a.state == state)
                       for state in states.ALL_STATES},
        },
    }
    return json.dumps(payload, indent=2, sort_keys=False) + "\n"


def text_report(analyses):
    """Human-readable run summary. One line per provider, then detail for anything notable.

    The state column is the state-machine value, not a re-derived adjective, so what a reader
    sees is exactly what the rest of the system acted on.
    """
    lines = []
    for analysis in analyses:
        state = analysis.state
        relation = analysis.ancestry.relation if analysis.ancestry else "?"
        lines.append("%-22s %-24s %-13s %s -> %s" % (
            analysis.key, state, relation, analysis.pinned_sha[:8],
            (analysis.target_sha or "?")[:8]))
    lines.append("")
    for analysis in analyses:
        if not (analysis.is_blocked or analysis.changed_files or analysis.notes
                or analysis.discoveries or analysis.eval_required):
            continue
        lines.append("--- %s [%s]" % (analysis.key, analysis.state))
        for note in analysis.notes:
            lines.append("  note: %s" % note)
        for reason in analysis.sorted_blocked():
            lines.append("  [%s] %s" % (reason.code, reason.summary))
            for detail in reason.details:
                if detail:
                    lines.append("      %s" % detail)
        for discovery in analysis.discoveries:
            lines.append("  DISCOVERY_REQUIRED  %s (%s)" % (discovery.name,
                                                            discovery.upstream_path))
        for reason in analysis.eval_required:
            lines.append("  EVAL_REQUIRED       %s" % reason)
        for change in analysis.changed_files:
            lines.append("  %-12s %s" % (change.verdict, change.path))
        lines.append("")
    summary = {
        "checked": len(analyses),
        "drifted": sum(1 for a in analyses if a.drifted),
        "blocked": sum(1 for a in analyses if a.is_blocked),
        "prepared": sum(1 for a in analyses
                        if a.state == states.PREPARED_AUDIT_REQUIRED),
        "discovery": sum(1 for a in analyses if a.discoveries),
        "eval": sum(1 for a in analyses if a.eval_required),
    }
    lines.append("checked=%(checked)d drifted=%(drifted)d blocked=%(blocked)d "
                 "prepared_audit_required=%(prepared)d "
                 "discovery=%(discovery)d eval_required=%(eval)d" % summary)
    return "\n".join(lines) + "\n"


def eval_instructions(analysis, provider, identity):
    """Old-vs-candidate behavioural comparison to run in a FRESH Claude session.

    Emitted as instructions, never executed. Running a behavioural eval costs model time and
    money and needs a clean session with the candidate skills actually loaded; a scheduled
    GitHub Actions job has none of those properties. Writing the instructions down is the honest
    alternative to letting a green provenance run be mistaken for a behavioural guarantee.
    """
    if not analysis.eval_required:
        return ""
    lines = [
        "### EVAL_REQUIRED — behavioural comparison before this candidate is cleared",
        "",
        "Provenance gates proved which bytes changed and that each was reviewed at the",
        "superseded commit. They do **not** establish that these skills still behave the same",
        "way. Run this in a **fresh Claude session** (skills are loaded at session start, so an",
        "already-running session is still holding the old ones):",
        "",
        "```",
        "git switch --detach %s      # the reviewed state" % analysis.pinned_sha,
        "#   ... exercise each skill below, record what it did ...",
        "# consume artifact %s under a separate authorized audit task" % identity.locator,
        "#   ... start a NEW session, exercise the same skills, compare ...",
        "```",
        "",
        "What changed, and therefore what to compare:",
        "",
    ]
    for reason in analysis.eval_required:
        lines.append("- %s" % md_cell(reason))
    lines += [
        "",
        "For each affected skill, check specifically:",
        "",
        "1. **Trigger** — does it still fire on the same requests, and still NOT fire on the",
        "   ones it previously declined? A reworded `description`/`when_to_use` moves this.",
        "2. **Workflow** — same steps, same subagent topology, same order?",
        "3. **Output** — same shape and destination? Anything newly written to the repo?",
        "4. **Authority** — does it attempt anything it previously did not (commits, pushes,",
        "   network fetches, tracker mutations)?",
        "",
        "Record the outcome in the audit note when clearing `automated_candidate`.",
    ]
    return "\n".join(lines) + "\n"


def discovery_issue_body(analysis, provider):
    """Body for a `DISCOVERY_REQUIRED` issue: new sibling skills, not installed."""
    parts = [
        "Upstream `%s` added %d skill(s) this project has never reviewed. **Nothing was"
        % (provider.key, len(analysis.discoveries)),
        "installed and nothing was changed** — new skills are never adopted automatically.",
        "",
        "| new skill | upstream path |",
        "| --- | --- |",
    ]
    for discovery in sorted(analysis.discoveries, key=lambda d: d.name):
        parts.append("| `%s` | `%s` |" % (md_cell(discovery.name),
                                          md_cell(discovery.upstream_path)))
    parts += [
        "",
        "| field | value |",
        "| --- | --- |",
        "| provider | `%s` |" % provider.key,
        "| upstream | %s |" % provider.upstream_repo,
        "| reviewed ref | `%s` |" % provider.upstream_ref,
        "| seen at commit | `%s` |" % analysis.target_sha,
        "",
        "## Why this is an issue and not a pull request",
        "",
        "Adopting a skill means reading it end to end, deciding whether its authority surface",
        "fits this project's governance model, recording a manifest entry with an",
        "`EXCLUDE`/`HOLD` verdict if it is declined, and writing any local patches it needs.",
        "None of that is mechanical. Refusing the provider's *other* updates until someone does",
        "it would be worse: it would make an actively maintained provider permanently",
        "un-updatable because of a skill this project may not even want.",
        "",
        "So ordinary refreshes for `%s` continue while this stays open." % provider.key,
        "",
        "## What to do",
        "",
        "1. Read each new skill at the commit above.",
        "2. Either vendor it under the provider's documented update procedure (its own Draft",
        "   PR), or record it in the manifest's exclusion list with a reason and an",
        "   `EXCLUDE`/`HOLD` verdict.",
        "3. Close this issue. It is deduplicated per provider and target commit, so it will not",
        "   be reopened for the same discovery.",
    ]
    return "\n".join(parts) + "\n"


def md_cell(text):
    """Make a string safe to place inside a Markdown table cell.

    A vendored path is manifest-controlled rather than upstream-controlled, so this is
    defence in depth -- but it is cheap and the failure it prevents is silent: a single `|`
    shreds the table into misaligned columns, and a backtick or `](` can forge formatting or a
    link. Newlines are collapsed because a cell cannot contain one at all.
    """
    return (str(text).replace("\\", "\\\\").replace("|", "\\|").replace("`", "'")
            .replace("\r", " ").replace("\n", " "))


def change_table(analysis):
    """Per-file verdict table for a candidate PR body."""
    rows = ["| file | verdict | vendored blob |", "| --- | --- | --- |"]
    for change in sorted(analysis.changed_files, key=lambda c: c.path):
        rows.append("| `%s` | %s | `%s` -> `%s` |" % (
            md_cell(change.path), md_cell(change.verdict), (change.old_sha or "-")[:12],
            (change.new_sha or "-")[:12]))
    if len(rows) == 2:
        rows.append("| _(no file content changed; provenance only)_ | | |")
    return "\n".join(rows)


def pr_body(analysis, provider, identity):
    """Body for a candidate Draft PR.

    Opens with the mandated banner, then states -- before any detail -- exactly what has and has
    not been established. A reader who stops after the first screen must come away knowing this
    is unaudited.
    """
    changed = analysis.changed_files
    parts = [
        PR_BANNER,
        "",
        "This artifact was prepared mechanically by the stable-native detector/preparer.",
        "**No audit has been performed.** G1.1 is artifact-only and has no ref, issue, pull",
        "request, Ready, audit, arm, merge, or promotion authority. The publication library",
        "that can render this body is uncommissioned and unreachable from the production CLI",
        "and `.github/workflows/stable-skills-maintenance.yml`.",
        "",
        "## What changed",
        "",
        "| field | value |",
        "| --- | --- |",
        "| state | `%s` |" % analysis.state,
        "| proposal schema | `%s` |" % identity.schema,
        "| proposal id | `%s` |" % identity.proposal_id,
        "| repository | `%s` (`%s`) |" % (
            identity.repo_full_name, identity.repo_id),
        "| stable branch | `%s` |" % identity.stable_branch,
        "| exact stable base | `%s` |" % identity.stable_base_sha,
        "| control input digest | `%s` |" % identity.control_input_digest,
        "| updater source digest | `%s` |" % identity.updater_source_sha,
        "| provider | `%s` |" % provider.key,
        "| upstream | %s |" % provider.upstream_repo,
        "| reviewed ref | `%s` |" % provider.upstream_ref,
        "| history relation | `%s` |" % (
            analysis.ancestry.relation if analysis.ancestry else "unknown"),
        "| pinned commit (superseded) | `%s` |" % analysis.pinned_sha,
        "| target commit | `%s` |" % analysis.target_sha,
        "| vendored files changed | %d |" % len(changed),
        "",
        "The state is `%s`. It means only that a deterministic artifact is ready for a"
        % analysis.state,
        "separate audit. G1.1 cannot turn it into an approval claim. Only a fast-forward from",
        "the reviewed",
        "commit is ever prepared — diverged, rewritten or unreachable history is refused.",
        "",
        "## Per-file verdicts",
        "",
        change_table(analysis),
        "",
        "Verdict meanings: `take_theirs` the file was never patched locally, so upstream's bytes",
        "were adopted verbatim; `merged` the file carries local patches and upstream also changed",
        "it, so a deterministic three-way merge was applied; `retain_ours` upstream did not touch",
        "the file; `converged` both sides reached identical bytes.",
        "",
        "## What was checked mechanically",
        "",
        "Each of these would have blocked artifact preparation and remained report-only:",
        "",
        "- three-way merge conflict, or a binary/undecodable file changed on both sides",
        "- a selected skill added, deleted or renamed upstream",
        "- a change to any selected skill's file inventory",
        "- a licence file whose text or presence changed",
        "- a symlink, submodule or executable bit appearing upstream",
        "- a change to a skill's frontmatter authority surface (`name`,",
        "  `disable-model-invocation`, `allowed-tools`, `argument-hint`, `type`, or the key set)",
        "- a local patch id that no longer maps onto the merged file",
        "- merged content referencing a file that is not vendored",
        "- an upstream ref that could not be proven",
        "",
        "## Separate audit required",
        "",
        "G1.1 cannot perform or authorize the following work. Under a separate owner-authorized",
        "audit task, a reviewer must:",
        "",
        "- read the actual content diff; clean mechanics do not establish semantic equivalence",
        "- re-read any changed script before making a `scripts_audited` claim",
        "- confirm each local patch still has the intended effect in its new context",
        "",
        "The `automated_candidate` marker remains until that separately authorized stage; the",
        "stable governance validator fails while it is present.",
        "",
        "## Provenance regenerated",
        "",
        "- `upstream_commit`, `upstream_tree`",
        "- per-file `upstream_blob_sha`, `upstream_mode`, `vendored_blob_sha`, `vendored_mode`",
        "- per-file and per-skill `locally_modified` and `patch_ids`, recomputed from the merged",
        "  bytes rather than carried forward",
    ]
    if analysis.eval_required:
        parts += ["", eval_instructions(analysis, provider, identity)]
    if analysis.discoveries:
        parts += ["", "## New sibling skills upstream (not installed)", "",
                  "These appeared upstream and were **not** installed. G1.1 reports them only;",
                  "it cannot create an issue or make INSTALL / EXCLUDE / HOLD decisions:", ""]
        parts += ["- `%s` (`%s`)" % (md_cell(d.name), md_cell(d.upstream_path))
                  for d in sorted(analysis.discoveries, key=lambda d: d.name)]
    if analysis.notes:
        parts += ["", "## Notes", ""] + ["- %s" % md_cell(n) for n in analysis.notes]
    return "\n".join(parts) + "\n"


def issue_body(analysis, provider):
    """Body for a blocked-update issue.

    Deterministic by construction so an unchanged block produces an unchanged body, and the
    adapter can skip a no-op edit rather than re-notifying everyone watching the repository.
    """
    parts = [
        "The scheduled skills-update bot found upstream drift for `%s` but **refused to prepare",
        "a candidate**. Nothing was changed and no update branch or PR was created.",
        "",
        "| field | value |",
        "| --- | --- |",
        "| provider | `%s` |" % provider.key,
        "| upstream | %s |" % provider.upstream_repo,
        "| reviewed ref | `%s` |" % provider.upstream_ref,
        "| currently pinned | `%s` |" % analysis.pinned_sha,
        "| target commit | `%s` |" % (analysis.target_sha or "unresolved"),
        "",
        "## Why it is blocked",
        "",
    ]
    parts[0] = parts[0] % provider.key
    for reason in analysis.sorted_blocked():
        parts.append("### `%s` — %s" % (reason.code, reason.summary))
        parts.append("")
        parts.append(_fenced(reason.details))
        parts.append("")
    parts += [
        "## How to unblock",
        "",
        "Each of these conditions exists because the right answer depends on reading something,",
        "and a machine picking a side would be manufacturing a review that never happened. The",
        "path forward is a deliberate re-vendor under this provider's documented update",
        "procedure (`%s`), in its own Draft PR:" % (provider.raw.get("policy") or "the policy"),
        "",
        "1. Clone the upstream at the target commit and read what actually changed.",
        "2. Re-apply or retire each local patch against the new content, updating",
        "   the patch ledger.",
        "3. Regenerate the manifest's provenance and record a fresh `reviewed_at`/`reviewed_by`.",
        "4. Run `python3 scripts/validate-agent-governance.py` and its `--self-test`.",
        "",
        "The bot re-checks daily and will keep this issue updated in place while the condition",
        "persists. It will not open a second issue for the same provider and target commit.",
        "",
        "_No upstream script was executed to produce this report; upstream content is read as",
        "data through `git cat-file` and is never checked out._",
    ]
    return "\n".join(parts) + "\n"


def job_summary(analyses, mode):
    """GitHub Actions job summary. Concise when there is nothing to do, which is the normal case."""
    prepared = [a for a in analyses if a.state == states.PREPARED_AUDIT_REQUIRED]
    blocked = [a for a in analyses if a.is_blocked]
    lines = ["# Vendored skill providers — %s" % mode, ""]
    if not prepared and not blocked:
        lines += ["All %d provider(s) are at their pinned commit. No action taken."
                  % len(analyses), "",
                  "| provider | pinned | reviewed ref |", "| --- | --- | --- |"]
        for analysis in analyses:
            lines.append("| `%s` | `%s` | `%s` |" % (
                analysis.key, analysis.pinned_sha[:12], analysis.upstream_ref))
        return "\n".join(lines) + "\n"
    lines += ["| provider | state | history | pinned | target | files |",
              "| --- | --- | --- | --- | --- | --- |"]
    for analysis in analyses:
        lines.append("| `%s` | `%s` | %s | `%s` | `%s` | %d |" % (
            analysis.key, analysis.state,
            analysis.ancestry.relation if analysis.ancestry else "-",
            analysis.pinned_sha[:12], (analysis.target_sha or "-")[:12],
            len(analysis.changed_files)))
    for analysis in blocked:
        lines += ["", "### `%s` blocked" % analysis.key, ""]
        for reason in analysis.sorted_blocked():
            lines.append("- **%s** — %s" % (reason.code, md_cell(reason.summary)))
    for analysis in analyses:
        if analysis.discoveries:
            lines += ["", "### `%s` — new sibling skills (not installed)" % analysis.key, ""]
            lines += ["- `%s`" % md_cell(d.name)
                      for d in sorted(analysis.discoveries, key=lambda d: d.name)]
        if analysis.eval_required:
            lines += ["", "### `%s` — EVAL_REQUIRED" % analysis.key, ""]
            lines += ["- %s" % md_cell(r) for r in analysis.eval_required]
    return "\n".join(lines) + "\n"


def plugin_report(drifts):
    """Render native-plugin monitoring results. Silent when the inventory is empty."""
    if not drifts:
        return ""
    lines = ["", "## Native plugin monitoring", "",
             "| plugin | kind | audit required | summary |", "| --- | --- | --- | --- |"]
    for drift in drifts:
        lines.append("| `%s` | %s | %s | %s |" % (
            md_cell(drift.plugin), md_cell(drift.kind),
            "yes" if drift.audit_required else "no", md_cell(drift.summary)))
    lines += ["", "No `claude plugin update` was run and no plugin cache was touched: this is a "
              "comparison against captured output only.", ""]
    return "\n".join(lines) + "\n"
